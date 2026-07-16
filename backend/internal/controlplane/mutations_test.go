package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
	_ "modernc.org/sqlite"
)

type fakeUpstream struct {
	mu               sync.Mutex
	version          string
	statusErr        error
	manageCalls      int
	createCalls      int
	deleteCalls      int
	onStatusContext  func(context.Context) error
	onManage         func(newapi.ManageUserRequest) error
	onManageContext  func(context.Context, newapi.ManageUserRequest) error
	onCreate         func(newapi.RedemptionCreateRequest) ([]string, error)
	onCreateContext  func(context.Context, newapi.RedemptionCreateRequest) ([]string, error)
	onDelete         func(int) error
	onDeleteContext  func(context.Context, int) error
	onHardDeleteUser func(int) error
}

func (f *fakeUpstream) Status(ctx context.Context) (*newapi.Status, error) {
	f.mu.Lock()
	onStatusContext := f.onStatusContext
	statusErr := f.statusErr
	version := f.version
	f.mu.Unlock()
	if onStatusContext != nil {
		if err := onStatusContext(ctx); err != nil {
			return nil, err
		}
	}
	if statusErr != nil {
		return nil, statusErr
	}
	return &newapi.Status{Version: version}, nil
}

func (f *fakeUpstream) ManageUser(ctx context.Context, request newapi.ManageUserRequest) error {
	f.mu.Lock()
	f.manageCalls++
	onManageContext := f.onManageContext
	onManage := f.onManage
	f.mu.Unlock()
	if onManageContext != nil {
		return onManageContext(ctx, request)
	}
	if onManage != nil {
		return onManage(request)
	}
	return nil
}

func (f *fakeUpstream) HardDeleteUser(_ context.Context, userID int, _ newapi.Capabilities) error {
	f.mu.Lock()
	f.manageCalls++
	onHardDeleteUser := f.onHardDeleteUser
	f.mu.Unlock()
	if onHardDeleteUser != nil {
		return onHardDeleteUser(userID)
	}
	return nil
}

func (f *fakeUpstream) CreateRedemptions(ctx context.Context, request newapi.RedemptionCreateRequest) ([]string, error) {
	f.mu.Lock()
	f.createCalls++
	onCreateContext := f.onCreateContext
	onCreate := f.onCreate
	f.mu.Unlock()
	if onCreateContext != nil {
		return onCreateContext(ctx, request)
	}
	if onCreate != nil {
		return onCreate(request)
	}
	return []string{"secret-key-a"}, nil
}

func (f *fakeUpstream) DeleteRedemption(ctx context.Context, id int) error {
	f.mu.Lock()
	f.deleteCalls++
	onDeleteContext := f.onDeleteContext
	onDelete := f.onDelete
	f.mu.Unlock()
	if onDeleteContext != nil {
		return onDeleteContext(ctx, id)
	}
	if onDelete != nil {
		return onDelete(id)
	}
	return nil
}

func testMeta() OperationMeta {
	return OperationMeta{
		RequestID:      "request-12345678",
		Actor:          "admin",
		ActorRole:      "admin",
		SourceIP:       "127.0.0.1",
		AuthMethod:     "jwt",
		Reason:         "confirmed operator action",
		IdempotencyKey: "operation-12345678",
	}
}

func TestUserMutationProtectsAdminAndRootTargets(t *testing.T) {
	tests := []struct {
		name       string
		targetID   int
		targetRole int
		actorRole  string
		action     string
		wantErr    error
		wantCalls  int
	}{
		{name: "operator cannot enable admin", targetID: 8, targetRole: 10, actorRole: "operator", action: "enable", wantErr: ErrAdminRoleRequired},
		{name: "operator cannot disable admin", targetID: 8, targetRole: 10, actorRole: "operator", action: "disable", wantErr: ErrAdminRoleRequired},
		{name: "operator cannot delete admin", targetID: 8, targetRole: 10, actorRole: "operator", action: "delete", wantErr: ErrAdminRoleRequired},
		{name: "admin can manage admin", targetID: 8, targetRole: 10, actorRole: "admin", action: "disable", wantCalls: 1},
		{name: "admin cannot disable root", targetID: 9, targetRole: 100, actorRole: "admin", action: "disable", wantErr: ErrProtectedRootTarget},
		{name: "admin cannot delete root", targetID: 9, targetRole: 100, actorRole: "admin", action: "delete", wantErr: ErrProtectedRootTarget},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
			service, store, db := setupMutationService(t, upstream)
			db.MustExec(`INSERT INTO users(id, status, role, deleted_at) VALUES (?, 1, ?, NULL)`, test.targetID, test.targetRole)
			meta := testMeta()
			meta.Actor = "role-test-" + test.actorRole
			meta.ActorRole = test.actorRole
			meta.IdempotencyKey = "protected-user-" + strconv.Itoa(test.targetID) + "-" + test.actorRole + "-" + test.action

			_, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: test.targetID, Action: test.action})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			upstream.mu.Lock()
			calls := upstream.manageCalls
			upstream.mu.Unlock()
			if calls != test.wantCalls {
				t.Fatalf("upstream calls = %d, want %d", calls, test.wantCalls)
			}
			if test.wantErr != nil {
				page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "user", TargetID: strconv.Itoa(test.targetID)})
				if listErr != nil || len(page.Items) != 0 {
					t.Fatalf("protected target wrote an audit intent: page=%+v err=%v", page.Items, listErr)
				}
			}
		})
	}
}

func TestMutationRoleIsBoundToIdempotentReplay(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, _, db := setupMutationService(t, upstream)
	upstream.onManage = func(request newapi.ManageUserRequest) error {
		_, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID)
		return err
	}
	meta := testMeta()
	meta.IdempotencyKey = "role-bound-replay"
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	meta.ActorRole = "operator"
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("role-changed replay error = %v, want ErrIdempotencyConflict", err)
	}
}

func setupMutationService(t *testing.T, upstream *fakeUpstream) (*Service, *toolstore.Store, *sqlx.DB) {
	return setupMutationServiceWithUpstream(t, upstream)
}

func setupMutationServiceWithUpstream(t *testing.T, upstream UpstreamClient) (*Service, *toolstore.Store, *sqlx.DB) {
	t.Helper()
	db, err := sqlx.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.MustExec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY,
		status INTEGER,
		role INTEGER,
		deleted_at INTEGER
	)`)
	db.MustExec(`INSERT INTO users(id, status, role, deleted_at) VALUES (7, 1, 1, NULL)`)
	store, err := toolstore.Init(":memory:")
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	service := NewService(upstream, store, &database.Manager{DB: db, IsPG: false}, observability.NewRegistry())
	t.Cleanup(func() {
		_ = store.Close()
		_ = db.Close()
	})
	return service, store, db
}

func TestUserMutationRecordsIntentBeforeUpstreamAndOutcomeAfter(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, db := setupMutationService(t, upstream)
	upstream.onManage = func(request newapi.ManageUserRequest) error {
		page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "user.disable.intent"})
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("intent was not durable before upstream call: page=%+v err=%v", page, err)
		}
		_, err = db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID)
		return err
	}

	result, err := service.MutateUser(context.Background(), testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if result.AuditID == 0 || result.Replayed {
		t.Fatalf("result = %+v", result)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "user", TargetID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].Action != "user.disable.outcome" || page.Items[0].Status != toolstore.OperationSucceeded {
		t.Fatalf("audit trail = %+v", page.Items)
	}
}

func TestUnknownNewAPIVersionIsDeniedWithoutMutation(t *testing.T) {
	upstream := &fakeUpstream{version: "v2.0.0"}
	service, store, _ := setupMutationService(t, upstream)
	_, err := service.MutateUser(context.Background(), testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if upstream.manageCalls != 0 {
		t.Fatalf("unknown version executed %d mutations", upstream.manageCalls)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "user.disable.outcome"})
	if listErr != nil || len(page.Items) != 1 || page.Items[0].Status != toolstore.OperationDenied {
		t.Fatalf("denied audit = %+v err=%v", page.Items, listErr)
	}
}

func TestUserMutationIdempotencyReplaysWithoutSecondUpstreamCall(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, _, db := setupMutationService(t, upstream)
	upstream.onManage = func(request newapi.ManageUserRequest) error {
		_, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID)
		return err
	}
	meta := testMeta()
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	second, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || upstream.manageCalls != 1 {
		t.Fatalf("replay=%+v calls=%d", second, upstream.manageCalls)
	}
}

func TestCrossServiceIntentClaimAllowsOnlyOneUpstreamCall(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onDeleteContext = func(ctx context.Context, _ int) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	firstService, store, db := setupMutationService(t, upstream)
	secondService := NewService(upstream, store, &database.Manager{DB: db, IsPG: false}, observability.NewRegistry())
	meta := testMeta()
	meta.IdempotencyKey = "cross-service-delete-claim"

	type deleteResponse struct {
		result RedemptionDeleteResult
		err    error
	}
	firstDone := make(chan deleteResponse, 1)
	go func() {
		result, err := firstService.DeleteRedemptions(context.Background(), meta, []int{101})
		firstDone <- deleteResponse{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first service did not reach upstream")
	}

	secondMeta := meta
	secondMeta.RequestID = "cross-service-request-2"
	secondDone := make(chan deleteResponse, 1)
	go func() {
		result, err := secondService.DeleteRedemptions(context.Background(), secondMeta, []int{101})
		secondDone <- deleteResponse{result: result, err: err}
	}()
	select {
	case <-started:
		releaseUpstream()
		t.Fatal("unclaimed cross-service request reached upstream")
	case response := <-secondDone:
		if !errors.Is(response.err, ErrOperationUncertain) {
			t.Fatalf("unclaimed cross-service response = %+v, err=%v; want ErrOperationUncertain", response.result, response.err)
		}
	case <-time.After(time.Second):
		releaseUpstream()
		t.Fatal("second service neither rejected the live intent nor reached upstream")
	}

	releaseUpstream()
	first := <-firstDone
	if first.err != nil || first.result.Deleted != 1 || first.result.Replayed {
		t.Fatalf("claimed service response = %+v, err=%v", first.result, first.err)
	}
	replayed, err := secondService.DeleteRedemptions(context.Background(), secondMeta, []int{101})
	if err != nil || !replayed.Replayed || replayed.Deleted != 1 {
		t.Fatalf("stable cross-service replay = %+v, err=%v", replayed, err)
	}
	upstream.mu.Lock()
	calls := upstream.deleteCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("cross-service upstream calls = %d, want 1", calls)
	}
}

func TestSameServiceIntentLockStillWaitsAndReplays(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onDeleteContext = func(ctx context.Context, _ int) error {
		started <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service, _, _ := setupMutationService(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "same-service-delete-claim"

	type deleteResponse struct {
		result RedemptionDeleteResult
		err    error
	}
	firstDone := make(chan deleteResponse, 1)
	go func() {
		result, err := service.DeleteRedemptions(context.Background(), meta, []int{101})
		firstDone <- deleteResponse{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first same-service request did not reach upstream")
	}

	secondMeta := meta
	secondMeta.RequestID = "same-service-request-2"
	secondDone := make(chan deleteResponse, 1)
	go func() {
		result, err := service.DeleteRedemptions(context.Background(), secondMeta, []int{101})
		secondDone <- deleteResponse{result: result, err: err}
	}()
	select {
	case <-started:
		releaseUpstream()
		t.Fatal("same-service idempotency lock allowed a second upstream call")
	case response := <-secondDone:
		releaseUpstream()
		t.Fatalf("same-service retry completed before the owner: %+v, err=%v", response.result, response.err)
	case <-time.After(100 * time.Millisecond):
	}

	releaseUpstream()
	first := <-firstDone
	if first.err != nil || first.result.Deleted != 1 || first.result.Replayed {
		t.Fatalf("same-service owner response = %+v, err=%v", first.result, first.err)
	}
	second := <-secondDone
	if second.err != nil || !second.result.Replayed || second.result.Deleted != 1 {
		t.Fatalf("same-service replay response = %+v, err=%v", second.result, second.err)
	}
	upstream.mu.Lock()
	calls := upstream.deleteCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("same-service upstream calls = %d, want 1", calls)
	}
}

func TestHardDeleteReplayDoesNotRequireTargetToStillExist(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0"}
	service, _, db := setupMutationService(t, upstream)
	upstream.onHardDeleteUser = func(userID int) error {
		_, err := db.Exec(`DELETE FROM users WHERE id = ?`, userID)
		return err
	}
	meta := testMeta()
	meta.IdempotencyKey = "hard-delete-replay-key"
	request := UserMutationRequest{UserID: 7, Action: "delete", HardDelete: true}
	if _, err := service.MutateUser(context.Background(), meta, request); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.MutateUser(context.Background(), meta, request)
	if err != nil || !replayed.Replayed {
		t.Fatalf("hard-delete replay=%+v error=%v", replayed, err)
	}
	upstream.mu.Lock()
	calls := upstream.manageCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("hard delete reached upstream %d times", calls)
	}
}

func TestUserMutationReplayRejectsDifferentStableRequest(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, _, db := setupMutationService(t, upstream)
	upstream.onManage = func(request newapi.ManageUserRequest) error {
		_, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID)
		return err
	}
	meta := testMeta()
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	meta.Reason = "a different operator justification"
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed request replay error = %v, want ErrIdempotencyConflict", err)
	}
	upstream.mu.Lock()
	calls := upstream.manageCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("changed request reached upstream: calls=%d", calls)
	}
}

func TestUserMutationReplayRequiresOutcomeToMatchOriginalIntent(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, _ := setupMutationService(t, upstream)
	meta := testMeta()
	requestFingerprint := map[string]any{
		"action": "user.disable", "target_type": "user", "target_id": "7", "reason": meta.Reason,
	}
	intent, err := newOperationIntent(requestFingerprint, map[string]any{"exists": true, "id": 7, "status": 1})
	if err != nil {
		t.Fatal(err)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	tampered, err := newOperationIntent(requestFingerprint, map[string]any{"exists": true, "id": 7, "status": 999})
	if err != nil {
		t.Fatal(err)
	}
	tamperedJSON, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	intentKey, outcomeKey := auditKeys(meta.IdempotencyKey)
	base := toolstore.OperationAuditInput{
		RequestID: meta.RequestID, Actor: meta.Actor, SourceIP: meta.SourceIP, AuthMethod: meta.AuthMethod,
		TargetType: "user", TargetID: "7", Reason: meta.Reason, Status: toolstore.OperationSucceeded,
	}
	intentInput := base
	intentInput.Action = "user.disable.intent"
	intentInput.BeforeJSON = intentJSON
	intentInput.IdempotencyKey = intentKey
	if _, err := store.AppendOperationAudit(context.Background(), intentInput); err != nil {
		t.Fatal(err)
	}
	outcomeInput := base
	outcomeInput.Action = "user.disable.outcome"
	outcomeInput.BeforeJSON = tamperedJSON
	outcomeInput.AfterJSON = json.RawMessage(`{"observed":true}`)
	outcomeInput.IdempotencyKey = outcomeKey
	if _, err := store.AppendOperationAudit(context.Background(), outcomeInput); err != nil {
		t.Fatal(err)
	}

	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("mismatched outcome replay error = %v, want ErrIdempotencyConflict", err)
	}
	upstream.mu.Lock()
	calls := upstream.manageCalls
	upstream.mu.Unlock()
	if calls != 0 {
		t.Fatalf("mismatched audit chain reached upstream: calls=%d", calls)
	}
}

func TestUserMutationTargetLockSerializesDifferentIdempotencyKeys(t *testing.T) {
	started := make(chan int, 2)
	releaseFirst := make(chan struct{})
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, _, db := setupMutationService(t, upstream)
	upstream.onManageContext = func(_ context.Context, request newapi.ManageUserRequest) error {
		upstream.mu.Lock()
		call := upstream.manageCalls
		upstream.mu.Unlock()
		started <- call
		if call == 1 {
			<-releaseFirst
		}
		_, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID)
		return err
	}

	errs := make(chan error, 2)
	firstMeta := testMeta()
	firstMeta.IdempotencyKey = "same-user-first-key"
	go func() {
		_, err := service.MutateUser(context.Background(), firstMeta, UserMutationRequest{UserID: 7, Action: "disable"})
		errs <- err
	}()
	select {
	case call := <-started:
		if call != 1 {
			t.Fatalf("first upstream call number = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first user mutation did not reach upstream")
	}

	secondMeta := testMeta()
	secondMeta.RequestID = "request-same-user-second"
	secondMeta.IdempotencyKey = "same-user-second-key"
	go func() {
		_, err := service.MutateUser(context.Background(), secondMeta, UserMutationRequest{UserID: 7, Action: "disable"})
		errs <- err
	}()
	select {
	case call := <-started:
		close(releaseFirst)
		t.Fatalf("same-target mutation reached upstream concurrently as call %d", call)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case call := <-started:
		if call != 2 {
			t.Fatalf("second upstream call number = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("second user mutation did not resume after target lock release")
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserMutationPersistsSuccessAfterCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, db := setupMutationService(t, upstream)
	upstream.onManageContext = func(_ context.Context, request newapi.ManageUserRequest) error {
		if _, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID); err != nil {
			return err
		}
		cancel()
		return nil
	}

	result, err := service.MutateUser(ctx, testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
	if err != nil || result.AuditID == 0 {
		t.Fatalf("detached outcome result=%+v error=%v", result, err)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "user.disable.outcome"})
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != toolstore.OperationSucceeded {
		t.Fatalf("detached success outcome=%+v error=%v", page.Items, err)
	}
}

func TestUserMutationCancellationPersistsUncertainOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, _ := setupMutationService(t, upstream)
	upstream.onManageContext = func(ctx context.Context, _ newapi.ManageUserRequest) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := service.MutateUser(ctx, testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("user mutation did not reach upstream")
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled user mutation error = %v", err)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "user.disable.outcome"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("canceled outcome=%+v error=%v", page.Items, err)
	}
	item := page.Items[0]
	if item.Status != toolstore.OperationCancelled || item.ErrorCode != "NEWAPI_TIMEOUT" ||
		!strings.Contains(string(item.AfterJSON), `"uncertain":true`) ||
		!strings.Contains(string(item.AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe canceled outcome: %+v", item)
	}
}

func TestUserMutationAppliesServiceDeadlineToCapabilityAndWriteCalls(t *testing.T) {
	t.Run("capability detection", func(t *testing.T) {
		upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
		upstream.onStatusContext = func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		service, _, _ := setupMutationService(t, upstream)
		service.userMutationTimeout = 25 * time.Millisecond
		service.outcomeAuditTimeout = 10 * time.Millisecond

		started := time.Now()
		_, err := service.MutateUser(context.Background(), testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("capability timeout error = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("capability timeout took %s, service deadline was not enforced", elapsed)
		}
	})

	t.Run("user write", func(t *testing.T) {
		upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
		upstream.onManageContext = func(ctx context.Context, _ newapi.ManageUserRequest) error {
			<-ctx.Done()
			return ctx.Err()
		}
		service, _, _ := setupMutationService(t, upstream)
		service.userMutationTimeout = 25 * time.Millisecond
		service.outcomeAuditTimeout = 10 * time.Millisecond

		started := time.Now()
		_, err := service.MutateUser(context.Background(), testMeta(), UserMutationRequest{UserID: 7, Action: "disable"})
		if !errors.Is(err, ErrOperationUncertain) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("write timeout error = %v, want uncertain deadline exceeded", err)
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("write timeout took %s, service deadline was not enforced", elapsed)
		}
	})
}

func TestUserMutationDeadlineIncludesTargetLockWaitAndReservesOutcomeAudit(t *testing.T) {
	var statusCalls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)

	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onStatusContext = func(ctx context.Context) error {
		if statusCalls.Add(1) == 1 {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	upstream.onManageContext = func(ctx context.Context, _ newapi.ManageUserRequest) error {
		close(firstStarted)
		select {
		case <-releaseFirst:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	service, store, _ := setupMutationService(t, upstream)
	service.userMutationTimeout = 2 * time.Second
	service.outcomeAuditTimeout = 100 * time.Millisecond
	firstMeta := testMeta()
	firstMeta.IdempotencyKey = "user-budget-lock-owner"
	firstDone := make(chan error, 1)
	go func() {
		_, err := service.MutateUser(context.Background(), firstMeta, UserMutationRequest{UserID: 7, Action: "disable"})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("lock owner did not reach the upstream user write")
	}

	// The owner has already captured its budget before this synchronized field
	// change. The queued request must count lock wait against this shorter total.
	service.userMutationTimeout = 250 * time.Millisecond
	service.outcomeAuditTimeout = 50 * time.Millisecond
	secondMeta := testMeta()
	secondMeta.RequestID = "request-user-budget-waiter"
	secondMeta.IdempotencyKey = "user-budget-lock-waiter"
	timer := time.AfterFunc(120*time.Millisecond, release)
	defer timer.Stop()

	started := time.Now()
	_, err := service.MutateUser(context.Background(), secondMeta, UserMutationRequest{UserID: 7, Action: "disable"})
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("queued capability timeout error = %v, want authoritative deadline exceeded", err)
	}
	if elapsed > 325*time.Millisecond {
		t.Fatalf("queued user mutation took %s; lock wait received a fresh upstream timeout", elapsed)
	}
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatalf("lock owner mutation error = %v", firstErr)
	}

	upstream.mu.Lock()
	manageCalls := upstream.manageCalls
	upstream.mu.Unlock()
	if manageCalls != 1 {
		t.Fatalf("queued timed-out mutation reached the upstream write: calls=%d", manageCalls)
	}
	_, outcomeKey := auditKeys(secondMeta.IdempotencyKey)
	outcome, auditErr := store.GetOperationAuditByIdempotencyKey(context.Background(), outcomeKey)
	if auditErr != nil {
		t.Fatalf("queued timeout outcome audit was not persisted from the reserved budget: %v", auditErr)
	}
	if outcome.Status != toolstore.OperationFailed || outcome.ErrorCode != "NEWAPI_STATUS_UNAVAILABLE" {
		t.Fatalf("queued timeout outcome audit = %+v", outcome)
	}
}

func TestUserMutationLostResponseRetryReturnsOperationUncertain(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, db := setupMutationService(t, upstream)
	upstream.onManage = func(request newapi.ManageUserRequest) error {
		if _, err := db.Exec(`UPDATE users SET status = 2 WHERE id = ?`, request.ID); err != nil {
			return err
		}
		// The write was applied, but no authoritative response reached the
		// control plane.
		return io.ErrUnexpectedEOF
	}
	meta := testMeta()
	meta.IdempotencyKey = "user-lost-response"

	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost-response error = %v, want unexpected EOF", err)
	}
	if _, err := service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"}); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("same-key retry error = %v, want ErrOperationUncertain", err)
	}
	upstream.mu.Lock()
	calls := upstream.manageCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("uncertain same-key retry reached upstream: calls=%d", calls)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "user.disable.outcome"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("lost-response outcome=%+v error=%v", page.Items, err)
	}
	item := page.Items[0]
	if item.Status != toolstore.OperationCancelled || item.ErrorCode != "NEWAPI_TRANSPORT_UNCERTAIN" ||
		!strings.Contains(string(item.AfterJSON), `"uncertain":true`) ||
		!strings.Contains(string(item.AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe lost-response outcome: %+v", item)
	}
}

func TestUserMutationHTTP502PersistsUncertainOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"version": "v1.0.0-rc.21"}})
		case "/api/user/manage":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"success":false,"message":"gateway failure"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	upstream, err := newapi.NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service, store, _ := setupMutationServiceWithUpstream(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "user-http-502"

	_, err = service.MutateUser(context.Background(), meta, UserMutationRequest{UserID: 7, Action: "disable"})
	if !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("502 user mutation error = %v, want ErrOperationUncertain", err)
	}
	assertUncertainOutcome(t, store, "user.disable.outcome")
}

func TestCreateRedemptionsInvalidSuccessJSONPersistsUncertainOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"version": "v1.0.0-rc.21"}})
		case "/api/redemption":
			_, _ = w.Write([]byte(`{"success":true,"data":`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	upstream, err := newapi.NewClient(server.URL, "token", 1, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service, store, _ := setupMutationServiceWithUpstream(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "redemption-invalid-json"

	_, err = service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{Name: "invalid-json", Count: 1, Quota: 10})
	if err == nil {
		t.Fatal("invalid redemption response was accepted")
	}
	assertUncertainOutcome(t, store, "redemption.create.outcome")
}

func TestCreateRedemptionsTruncatedKeysPersistsUncertainOutcome(t *testing.T) {
	upstream := &fakeUpstream{
		version: "v1.0.0-rc.21",
		onCreate: func(newapi.RedemptionCreateRequest) ([]string, error) {
			return []string{"secret-key-only-one"}, nil
		},
	}
	service, store, _ := setupMutationService(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "redemption-truncated-keys"
	request := newapi.RedemptionCreateRequest{Name: "truncated", Count: 2, Quota: 10}

	if result, err := service.CreateRedemptions(context.Background(), meta, request); !errors.Is(err, ErrOperationUncertain) || !errors.Is(err, newapi.ErrAmbiguousRedemptionKeys) || len(result.Keys) != 0 {
		t.Fatalf("truncated response result=%+v error=%v", result, err)
	}
	if _, err := service.CreateRedemptions(context.Background(), meta, request); !errors.Is(err, ErrOperationUncertain) {
		t.Fatalf("same-key retry error = %v, want ErrOperationUncertain", err)
	}
	assertUncertainOutcome(t, store, "redemption.create.outcome")
}

func assertUncertainOutcome(t *testing.T, store *toolstore.Store, action string) {
	t.Helper()
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: action})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("uncertain %s outcome=%+v error=%v", action, page.Items, err)
	}
	item := page.Items[0]
	if item.Status != toolstore.OperationCancelled || item.ErrorCode != "NEWAPI_TRANSPORT_UNCERTAIN" ||
		!strings.Contains(string(item.AfterJSON), `"uncertain":true`) ||
		!strings.Contains(string(item.AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe %s outcome: %+v", action, item)
	}
}

func TestRedemptionAuditNeverPersistsGeneratedKeys(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21", onCreate: func(newapi.RedemptionCreateRequest) ([]string, error) {
		return []string{"secret-key-a", "secret-key-b"}, nil
	}}
	service, store, _ := setupMutationService(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "redemption-create-1234"
	result, err := service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{
		Name: "customer-credit", Count: 2, Quota: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Keys) != 2 {
		t.Fatalf("keys = %v", result.Keys)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "redemption_batch"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(page.Items)
	if strings.Contains(string(encoded), "secret-key") {
		t.Fatalf("generated key leaked into audit: %s", encoded)
	}
}

func TestAppliedOperationReportsOutcomeAuditFailure(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, _ := setupMutationService(t, upstream)
	upstream.onCreate = func(newapi.RedemptionCreateRequest) ([]string, error) {
		_ = store.Close()
		return []string{"secret-key-a"}, nil
	}
	meta := testMeta()
	meta.IdempotencyKey = "redemption-audit-fail"
	result, err := service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{Name: "batch", Count: 1, Quota: 10})
	var appliedErr *AppliedButUnauditedError
	if !errors.As(err, &appliedErr) || len(result.Keys) != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	metrics := operationMetricsText(t, service.metrics)
	if !strings.Contains(metrics, `new_api_tools_control_operations_total{action="redemption.create",result="audit_error"} 1`) {
		t.Fatalf("audit failure metric missing:\n%s", metrics)
	}
	if strings.Contains(metrics, `new_api_tools_control_operations_total{action="redemption.create",result="success"}`) {
		t.Fatalf("success metric incremented before outcome audit persisted:\n%s", metrics)
	}
}

func TestCreateRedemptionsRejectsMoreThanOneUpstreamBatch(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	service, store, _ := setupMutationService(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "redemption-too-large"

	_, err := service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{
		Name: "too-large", Count: newapi.MaxRedemptionCreateCount + 1, Quota: 10,
	})
	if !errors.Is(err, ErrRedemptionBatchTooLarge) {
		t.Fatalf("error = %v, want ErrRedemptionBatchTooLarge", err)
	}
	upstream.mu.Lock()
	calls := upstream.createCalls
	upstream.mu.Unlock()
	if calls != 0 {
		t.Fatalf("oversized request reached upstream %d times", calls)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{TargetType: "redemption_batch"})
	if listErr != nil || len(page.Items) != 0 {
		t.Fatalf("oversized validation should precede intent audit: page=%+v err=%v", page, listErr)
	}
}

func TestCreateRedemptionsHasBoundedDeadlineAndAuditsTimeout(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onCreateContext = func(ctx context.Context, _ newapi.RedemptionCreateRequest) ([]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	service, store, _ := setupMutationService(t, upstream)
	service.redemptionCreateTimeout = 100 * time.Millisecond
	service.redemptionAuditReserve = 40 * time.Millisecond
	meta := testMeta()
	meta.IdempotencyKey = "redemption-timeout"

	started := time.Now()
	_, err := service.CreateRedemptions(context.Background(), meta, newapi.RedemptionCreateRequest{Name: "timeout", Count: 1, Quota: 10})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded operation took %s", elapsed)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "redemption.create.outcome"})
	if listErr != nil || len(page.Items) != 1 {
		t.Fatalf("timeout outcome audit = %+v err=%v", page.Items, listErr)
	}
	if page.Items[0].Status != toolstore.OperationCancelled || page.Items[0].ErrorCode != "NEWAPI_TIMEOUT" {
		t.Fatalf("timeout outcome = %+v", page.Items[0])
	}
	if strings.Contains(string(page.Items[0].AfterJSON), "secret-key") || !strings.Contains(string(page.Items[0].AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe timeout audit payload: %s", page.Items[0].AfterJSON)
	}
}

func TestDeleteRedemptionsCancellationPersistsUncertainOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onDeleteContext = func(ctx context.Context, _ int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	service, store, _ := setupMutationService(t, upstream)
	meta := testMeta()
	meta.IdempotencyKey = "redemption-delete-cancel"

	errCh := make(chan error, 1)
	go func() {
		_, err := service.DeleteRedemptions(ctx, meta, []int{101})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("redemption deletion did not reach upstream")
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled deletion error = %v", err)
	}
	page, err := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "redemption.delete.outcome"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("canceled deletion outcome=%+v error=%v", page.Items, err)
	}
	item := page.Items[0]
	if item.Status != toolstore.OperationCancelled || item.ErrorCode != "NEWAPI_TIMEOUT" ||
		!strings.Contains(string(item.AfterJSON), `"uncertain":true`) ||
		!strings.Contains(string(item.AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe canceled deletion outcome: %+v", item)
	}
}

func TestDeleteRedemptionsHasBoundedWholeOperationDeadline(t *testing.T) {
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onDeleteContext = func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	service, store, _ := setupMutationService(t, upstream)
	service.redemptionDeleteTimeout = 100 * time.Millisecond
	service.redemptionAuditReserve = 40 * time.Millisecond
	meta := testMeta()
	meta.IdempotencyKey = "redemption-delete-timeout"

	started := time.Now()
	_, err := service.DeleteRedemptions(context.Background(), meta, []int{101, 102})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("bounded deletion took %s", elapsed)
	}
	upstream.mu.Lock()
	calls := upstream.deleteCalls
	upstream.mu.Unlock()
	if calls != 1 {
		t.Fatalf("whole-operation deadline allowed %d delete calls, want 1", calls)
	}
	page, listErr := store.ListOperationAudits(context.Background(), toolstore.OperationAuditFilter{Action: "redemption.delete.outcome"})
	if listErr != nil || len(page.Items) != 1 {
		t.Fatalf("timeout deletion outcome=%+v error=%v", page.Items, listErr)
	}
	item := page.Items[0]
	if item.Status != toolstore.OperationCancelled || item.ErrorCode != "NEWAPI_TIMEOUT" ||
		!strings.Contains(string(item.AfterJSON), `"uncertain":true`) ||
		!strings.Contains(string(item.AfterJSON), `"do_not_retry":true`) {
		t.Fatalf("unsafe timeout deletion outcome: %+v", item)
	}
}

func TestDifferentIdempotencyKeysDoNotHoldGlobalMutationLock(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	upstream := &fakeUpstream{version: "v1.0.0-rc.21"}
	upstream.onCreateContext = func(ctx context.Context, request newapi.RedemptionCreateRequest) ([]string, error) {
		started <- request.Name
		select {
		case <-release:
			return []string{"key-" + request.Name}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	service, _, _ := setupMutationService(t, upstream)

	errs := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		meta := testMeta()
		meta.RequestID = "request-concurrent-" + name
		meta.IdempotencyKey = "redemption-concurrent-" + name
		request := newapi.RedemptionCreateRequest{Name: name, Count: 1, Quota: 10}
		go func(meta OperationMeta, request newapi.RedemptionCreateRequest) {
			_, err := service.CreateRedemptions(context.Background(), meta, request)
			errs <- err
		}(meta, request)
	}

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case name := <-started:
			seen[name] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatalf("only reached upstream for %v; operations are still globally serialized", seen)
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func operationMetricsText(t *testing.T, registry *observability.Registry) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/metrics", registry.Handler("test-token"))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", recorder.Code)
	}
	return recorder.Body.String()
}
