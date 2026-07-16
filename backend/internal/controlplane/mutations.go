package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/new-api-tools/backend/internal/database"
	"github.com/new-api-tools/backend/internal/newapi"
	"github.com/new-api-tools/backend/internal/observability"
	"github.com/new-api-tools/backend/internal/toolstore"
)

var (
	ErrAuditUnavailable             = errors.New("controlplane: audit store unavailable")
	ErrCapabilityUnavailable        = errors.New("controlplane: upstream capability unavailable")
	ErrIdempotencyRequired          = errors.New("controlplane: idempotency key required")
	ErrIdempotencyConflict          = errors.New("controlplane: idempotency key conflicts with another operation")
	ErrOperationUncertain           = errors.New("controlplane: operation outcome is uncertain; reconcile before retrying")
	ErrPreviousOperationFailed      = errors.New("controlplane: prior operation failed")
	ErrReplayRequiresReconciliation = errors.New("controlplane: operation already succeeded; reconcile the original result")
	ErrReasonRequired               = errors.New("controlplane: operation reason required")
	ErrRedemptionBatchTooLarge      = errors.New("controlplane: redemption creation is limited to 100 codes per operation")
	ErrOperatorRoleRequired         = errors.New("controlplane: operator role is required for mutations")
	ErrAdminRoleRequired            = errors.New("controlplane: admin role is required for this mutation")
	ErrProtectedRootTarget          = errors.New("controlplane: NewAPI root user is protected")
	ErrTargetNotFound               = errors.New("controlplane: target not found")
)

const (
	defaultRedemptionCreateTimeout       = 50 * time.Second
	defaultRedemptionDeleteTimeout       = 50 * time.Second
	defaultRedemptionAuditReserve        = 5 * time.Second
	defaultOutcomeAuditTimeout           = 5 * time.Second
	newAPIAdminRole                int64 = 10
	newAPIRootRole                 int64 = 100
)

type AppliedButUnauditedError struct {
	Action string
}

func (e *AppliedButUnauditedError) Error() string {
	return "controlplane: upstream operation applied but outcome audit could not be persisted"
}

type UpstreamClient interface {
	Status(context.Context) (*newapi.Status, error)
	ManageUser(context.Context, newapi.ManageUserRequest) error
	HardDeleteUser(context.Context, int, newapi.Capabilities) error
	CreateRedemptions(context.Context, newapi.RedemptionCreateRequest) ([]string, error)
	DeleteRedemption(context.Context, int) error
}

type OperationMeta struct {
	RequestID      string
	Actor          string
	ActorRole      string
	SourceIP       string
	AuthMethod     string
	Reason         string
	IdempotencyKey string
}

type UserMutationRequest struct {
	UserID     int
	Action     string
	HardDelete bool
}

type MutationResult struct {
	Action   string `json:"action"`
	TargetID string `json:"target_id"`
	Replayed bool   `json:"replayed"`
	AuditID  int64  `json:"audit_id,omitempty"`
}

type RedemptionCreateResult struct {
	Keys     []string `json:"keys"`
	Count    int      `json:"count"`
	Replayed bool     `json:"replayed"`
	AuditID  int64    `json:"audit_id,omitempty"`
}

type RedemptionDeleteResult struct {
	Requested int   `json:"requested"`
	Deleted   int   `json:"deleted"`
	IDs       []int `json:"ids"`
	Replayed  bool  `json:"replayed"`
	AuditID   int64 `json:"audit_id,omitempty"`
}

// operationIntent keeps the stable client request separate from any mutable
// pre-operation snapshot. Idempotency comparisons use Request, while the
// complete envelope remains the immutable BeforeJSON stored on both the intent
// and outcome audit records.
type operationIntent struct {
	Request json.RawMessage `json:"request"`
	Before  any             `json:"before,omitempty"`
}

type keyedMutationLock struct {
	semaphore chan struct{}
	refs      int
}

// mutationLockSet serializes requests by narrowly scoped keys. Mutations always
// use an idempotency key lock, and resources that cannot safely accept
// concurrent writes (currently users) additionally use a target lock.
type mutationLockSet struct {
	mu    sync.Mutex
	locks map[string]*keyedMutationLock
}

func (s *mutationLockSet) lockAll(ctx context.Context, keys ...string) (func(), error) {
	unique := make(map[string]struct{}, len(keys))
	ordered := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)

	unlocks := make([]func(), 0, len(ordered))
	for _, key := range ordered {
		unlock, err := s.lock(ctx, key)
		if err != nil {
			for index := len(unlocks) - 1; index >= 0; index-- {
				unlocks[index]()
			}
			return nil, err
		}
		unlocks = append(unlocks, unlock)
	}
	return func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}, nil
}

func (s *mutationLockSet) lock(ctx context.Context, key string) (func(), error) {
	key = strings.TrimSpace(key)
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*keyedMutationLock)
	}
	entry := s.locks[key]
	if entry == nil {
		entry = &keyedMutationLock{semaphore: make(chan struct{}, 1)}
		s.locks[key] = entry
	}
	entry.refs++
	s.mu.Unlock()

	select {
	case entry.semaphore <- struct{}{}:
		return func() {
			<-entry.semaphore
			s.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(s.locks, key)
			}
			s.mu.Unlock()
		}, nil
	case <-ctx.Done():
		s.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.locks, key)
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

type Service struct {
	upstream UpstreamClient
	store    *toolstore.Store
	mainDB   *database.Manager
	metrics  *observability.Registry
	now      func() time.Time

	mutationLocks           mutationLockSet
	redemptionLimits        RedemptionLimits
	redemptionCreateTimeout time.Duration
	redemptionDeleteTimeout time.Duration
	redemptionAuditReserve  time.Duration
	outcomeAuditTimeout     time.Duration
}

func NewService(upstream UpstreamClient, store *toolstore.Store, mainDB *database.Manager, metrics *observability.Registry, configuredLimits ...RedemptionLimits) *Service {
	if metrics == nil {
		metrics = observability.Default
	}
	limits := DefaultRedemptionLimits()
	if len(configuredLimits) > 0 {
		limits = configuredLimits[0].normalized()
	}
	return &Service{
		upstream: upstream, store: store, mainDB: mainDB, metrics: metrics, now: time.Now,
		redemptionLimits:        limits,
		redemptionCreateTimeout: defaultRedemptionCreateTimeout,
		redemptionDeleteTimeout: defaultRedemptionDeleteTimeout,
		redemptionAuditReserve:  defaultRedemptionAuditReserve,
		outcomeAuditTimeout:     defaultOutcomeAuditTimeout,
	}
}

func (s *Service) MutateUser(ctx context.Context, meta OperationMeta, request UserMutationRequest) (MutationResult, error) {
	if request.UserID <= 0 {
		return MutationResult{}, fmt.Errorf("user id must be positive")
	}
	if request.Action != "enable" && request.Action != "disable" && request.Action != "delete" {
		return MutationResult{}, fmt.Errorf("unsupported user action %q", request.Action)
	}
	if err := validateMeta(meta, true); err != nil {
		return MutationResult{}, err
	}
	action := "user." + request.Action
	if request.HardDelete {
		if request.Action != "delete" {
			return MutationResult{}, errors.New("hard delete is valid only for delete operations")
		}
		action = "user.hard_delete"
	}
	targetID := strconv.Itoa(request.UserID)

	unlock, err := s.mutationLocks.lockAll(ctx,
		"idempotency:"+strings.TrimSpace(meta.IdempotencyKey),
		"target:user:"+targetID,
	)
	if err != nil {
		return MutationResult{}, err
	}
	defer unlock()

	requestIntent := map[string]any{
		"action":      action,
		"target_type": "user",
		"target_id":   targetID,
		"reason":      strings.TrimSpace(meta.Reason),
		"actor_role":  normalizeActorRole(meta.ActorRole),
	}
	intent, err := newOperationIntent(requestIntent, nil)
	if err != nil {
		return MutationResult{}, err
	}
	replay, auditID, foundAudit, err := s.lookupOperation(ctx, meta, action, "user", targetID, intent.Request)
	if err != nil {
		return MutationResult{}, err
	}
	if foundAudit && replay {
		return MutationResult{Action: action, TargetID: targetID, Replayed: true, AuditID: auditID}, nil
	}

	before, found, err := s.readUserState(ctx, request.UserID)
	if err != nil {
		return MutationResult{}, err
	}
	if !found {
		return MutationResult{}, ErrTargetNotFound
	}
	intent.Before = before
	targetRole, ok := before["role"].(int64)
	if !ok {
		return MutationResult{}, errors.New("user audit state is missing a valid role")
	}
	if targetRole >= newAPIRootRole {
		return MutationResult{}, ErrProtectedRootTarget
	}
	if targetRole >= newAPIAdminRole && normalizeActorRole(meta.ActorRole) != "admin" {
		return MutationResult{}, ErrAdminRoleRequired
	}
	replay, auditID, err = s.beginOperation(ctx, meta, action, "user", targetID, intent)
	if err != nil {
		return MutationResult{}, err
	}
	if replay {
		return MutationResult{Action: action, TargetID: targetID, Replayed: true, AuditID: auditID}, nil
	}

	capabilities, version, err := s.detectCapabilities(ctx)
	if err != nil {
		auditCtx, cancel := s.newOutcomeContext(ctx)
		defer cancel()
		return MutationResult{}, s.finishFailure(auditCtx, meta, action, "user", targetID, intent, "NEWAPI_STATUS_UNAVAILABLE", toolstore.OperationFailed, err)
	}
	if !capabilities.AdminUserManage || (request.HardDelete && !capabilities.HardDeleteSafe) {
		denied := fmt.Errorf("%w: NewAPI %s is read-only for %s", ErrCapabilityUnavailable, version, action)
		auditCtx, cancel := s.newOutcomeContext(ctx)
		defer cancel()
		return MutationResult{}, s.finishFailure(auditCtx, meta, action, "user", targetID, intent, "NEWAPI_CAPABILITY_UNAVAILABLE", toolstore.OperationDenied, denied)
	}

	if request.HardDelete {
		err = s.upstream.HardDeleteUser(ctx, request.UserID, capabilities)
	} else {
		err = s.upstream.ManageUser(ctx, newapi.ManageUserRequest{ID: request.UserID, Action: request.Action})
	}
	auditCtx, cancel := s.newOutcomeContext(ctx)
	defer cancel()
	if err != nil {
		uncertain := isUncertainUpstreamError(err)
		status := toolstore.OperationFailed
		if uncertain {
			status = toolstore.OperationCancelled
		}
		outcome := map[string]any{
			"upstream_version": version,
			"uncertain":        uncertain,
			"do_not_retry":     uncertain,
		}
		if _, auditErr := s.appendOutcome(auditCtx, meta, action, "user", targetID, intent, outcome, status, upstreamErrorCode(err)); auditErr != nil {
			s.metrics.IncOperation(action, "audit_error")
			return MutationResult{}, ErrOperationUncertain
		}
		s.metrics.IncOperation(action, "error")
		if uncertain {
			return MutationResult{}, errors.Join(ErrOperationUncertain, err)
		}
		return MutationResult{}, err
	}

	after, observed, readErr := s.readUserState(auditCtx, request.UserID)
	if request.HardDelete && readErr == nil && !observed {
		after = map[string]any{"exists": false}
		observed = true
	}
	outcome := map[string]any{
		"upstream_version": version,
		"observed":         observed && readErr == nil,
		"after":            after,
	}
	audit, auditErr := s.appendOutcome(auditCtx, meta, action, "user", targetID, intent, outcome, toolstore.OperationSucceeded, "")
	if auditErr != nil {
		s.metrics.IncOperation(action, "audit_error")
		return MutationResult{}, &AppliedButUnauditedError{Action: action}
	}
	s.metrics.IncOperation(action, "success")
	return MutationResult{Action: action, TargetID: targetID, AuditID: audit.ID}, nil
}

func (s *Service) CreateRedemptions(ctx context.Context, meta OperationMeta, request newapi.RedemptionCreateRequest) (RedemptionCreateResult, error) {
	if err := validateMeta(meta, true); err != nil {
		return RedemptionCreateResult{}, err
	}
	if normalizeActorRole(meta.ActorRole) != "admin" {
		return RedemptionCreateResult{}, ErrAdminRoleRequired
	}
	if request.Count > newapi.MaxRedemptionCreateCount {
		return RedemptionCreateResult{}, ErrRedemptionBatchTooLarge
	}
	if strings.TrimSpace(request.Name) == "" || request.Count < 1 || request.Quota <= 0 || request.ExpiredTime < 0 {
		return RedemptionCreateResult{}, errors.New("invalid redemption creation request")
	}
	if err := s.redemptionLimits.validate(request.Count, request.Quota); err != nil {
		return RedemptionCreateResult{}, err
	}

	operationCtx, auditCtx, upstreamCtx, cancelOperation := s.redemptionOperationContexts(
		ctx, s.redemptionCreateTimeout, defaultRedemptionCreateTimeout,
	)
	defer cancelOperation()

	action := "redemption.create"
	targetID := strings.TrimSpace(request.Name)
	requestIntent := map[string]any{
		"action":       action,
		"target_type":  "redemption_batch",
		"target_id":    targetID,
		"reason":       strings.TrimSpace(meta.Reason),
		"actor_role":   normalizeActorRole(meta.ActorRole),
		"name":         request.Name,
		"count":        request.Count,
		"quota":        request.Quota,
		"expired_time": request.ExpiredTime,
	}
	intent, err := newOperationIntent(requestIntent, nil)
	if err != nil {
		return RedemptionCreateResult{}, err
	}

	unlock, err := s.mutationLocks.lock(operationCtx, "idempotency:"+strings.TrimSpace(meta.IdempotencyKey))
	if err != nil {
		return RedemptionCreateResult{}, err
	}
	defer unlock()
	replay, _, err := s.beginOperation(operationCtx, meta, action, "redemption_batch", targetID, intent)
	if err != nil {
		return RedemptionCreateResult{}, err
	}
	if replay {
		return RedemptionCreateResult{}, ErrReplayRequiresReconciliation
	}

	capabilities, version, err := s.detectCapabilities(upstreamCtx)
	if err != nil {
		return RedemptionCreateResult{}, s.finishFailure(auditCtx, meta, action, "redemption_batch", targetID, intent, "NEWAPI_STATUS_UNAVAILABLE", toolstore.OperationFailed, err)
	}
	if !capabilities.RedemptionAPI {
		denied := fmt.Errorf("%w: NewAPI %s redemption API is unavailable", ErrCapabilityUnavailable, version)
		return RedemptionCreateResult{}, s.finishFailure(auditCtx, meta, action, "redemption_batch", targetID, intent, "NEWAPI_CAPABILITY_UNAVAILABLE", toolstore.OperationDenied, denied)
	}

	keys, err := s.upstream.CreateRedemptions(upstreamCtx, request)
	if err == nil {
		err = newapi.ValidateRedemptionKeys(keys, request.Count)
	}
	if err != nil {
		uncertain := isUncertainUpstreamError(err)
		status := toolstore.OperationFailed
		if uncertain {
			status = toolstore.OperationCancelled
		}
		outcome := map[string]any{
			"upstream_version": version,
			"created_count":    0,
			"partial":          false,
			"uncertain":        uncertain,
			"do_not_retry":     uncertain,
		}
		if _, auditErr := s.appendOutcome(auditCtx, meta, action, "redemption_batch", targetID, intent, outcome, status, upstreamErrorCode(err)); auditErr != nil {
			s.metrics.IncOperation(action, "audit_error")
			return RedemptionCreateResult{}, ErrOperationUncertain
		}
		s.metrics.IncOperation(action, "error")
		if uncertain {
			return RedemptionCreateResult{}, errors.Join(ErrOperationUncertain, err)
		}
		return RedemptionCreateResult{}, err
	}
	// Redemption keys are intentionally returned to the caller but never copied
	// into the immutable audit database.
	outcome := map[string]any{"upstream_version": version, "created_count": len(keys)}
	audit, auditErr := s.appendOutcome(auditCtx, meta, action, "redemption_batch", targetID, intent, outcome, toolstore.OperationSucceeded, "")
	if auditErr != nil {
		s.metrics.IncOperation(action, "audit_error")
		return RedemptionCreateResult{Keys: keys, Count: len(keys)}, &AppliedButUnauditedError{Action: action}
	}
	s.metrics.IncOperation(action, "success")
	return RedemptionCreateResult{Keys: keys, Count: len(keys), AuditID: audit.ID}, nil
}

func (s *Service) DeleteRedemptions(ctx context.Context, meta OperationMeta, ids []int) (RedemptionDeleteResult, error) {
	if err := validateMeta(meta, true); err != nil {
		return RedemptionDeleteResult{}, err
	}
	if len(ids) == 0 || len(ids) > 100 {
		return RedemptionDeleteResult{}, errors.New("redemption ids must contain between 1 and 100 items")
	}
	seen := make(map[int]struct{}, len(ids))
	cleanIDs := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return RedemptionDeleteResult{}, errors.New("redemption ids must be positive")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		cleanIDs = append(cleanIDs, id)
	}
	action := "redemption.delete"
	targetID := joinIntIDs(cleanIDs)
	requestIntent := map[string]any{
		"action":      action,
		"target_type": "redemption_batch",
		"target_id":   targetID,
		"reason":      strings.TrimSpace(meta.Reason),
		"actor_role":  normalizeActorRole(meta.ActorRole),
		"ids":         cleanIDs,
	}
	intent, err := newOperationIntent(requestIntent, nil)
	if err != nil {
		return RedemptionDeleteResult{}, err
	}
	operationCtx, auditCtx, upstreamCtx, cancelOperation := s.redemptionOperationContexts(
		ctx, s.redemptionDeleteTimeout, defaultRedemptionDeleteTimeout,
	)
	defer cancelOperation()

	unlock, err := s.mutationLocks.lock(operationCtx, "idempotency:"+strings.TrimSpace(meta.IdempotencyKey))
	if err != nil {
		return RedemptionDeleteResult{}, err
	}
	defer unlock()
	replay, auditID, err := s.beginOperation(operationCtx, meta, action, "redemption_batch", targetID, intent)
	if err != nil {
		return RedemptionDeleteResult{}, err
	}
	if replay {
		return RedemptionDeleteResult{Requested: len(cleanIDs), Deleted: len(cleanIDs), IDs: cleanIDs, Replayed: true, AuditID: auditID}, nil
	}

	capabilities, version, err := s.detectCapabilities(upstreamCtx)
	if err != nil {
		return RedemptionDeleteResult{}, s.finishFailure(auditCtx, meta, action, "redemption_batch", targetID, intent, "NEWAPI_STATUS_UNAVAILABLE", toolstore.OperationFailed, err)
	}
	if !capabilities.RedemptionAPI {
		denied := fmt.Errorf("%w: NewAPI %s redemption API is unavailable", ErrCapabilityUnavailable, version)
		return RedemptionDeleteResult{}, s.finishFailure(auditCtx, meta, action, "redemption_batch", targetID, intent, "NEWAPI_CAPABILITY_UNAVAILABLE", toolstore.OperationDenied, denied)
	}

	deleted := 0
	for _, id := range cleanIDs {
		if err := s.upstream.DeleteRedemption(upstreamCtx, id); err != nil {
			uncertain := isUncertainUpstreamError(err)
			status := toolstore.OperationFailed
			if uncertain {
				status = toolstore.OperationCancelled
			}
			outcome := map[string]any{
				"upstream_version": version, "requested": len(cleanIDs), "deleted": deleted, "failed_id": id,
				"uncertain": uncertain, "do_not_retry": uncertain,
			}
			_, auditErr := s.appendOutcome(auditCtx, meta, action, "redemption_batch", targetID, intent, outcome, status, upstreamErrorCode(err))
			if auditErr != nil {
				s.metrics.IncOperation(action, "audit_error")
				return RedemptionDeleteResult{Requested: len(cleanIDs), Deleted: deleted, IDs: cleanIDs}, ErrOperationUncertain
			}
			s.metrics.IncOperation(action, "error")
			if uncertain {
				return RedemptionDeleteResult{Requested: len(cleanIDs), Deleted: deleted, IDs: cleanIDs}, errors.Join(ErrOperationUncertain, err)
			}
			return RedemptionDeleteResult{Requested: len(cleanIDs), Deleted: deleted, IDs: cleanIDs}, err
		}
		deleted++
	}
	outcome := map[string]any{"upstream_version": version, "requested": len(cleanIDs), "deleted": deleted}
	audit, auditErr := s.appendOutcome(auditCtx, meta, action, "redemption_batch", targetID, intent, outcome, toolstore.OperationSucceeded, "")
	result := RedemptionDeleteResult{Requested: len(cleanIDs), Deleted: deleted, IDs: cleanIDs}
	if auditErr != nil {
		s.metrics.IncOperation(action, "audit_error")
		return result, &AppliedButUnauditedError{Action: action}
	}
	s.metrics.IncOperation(action, "success")
	result.AuditID = audit.ID
	return result, nil
}

func (s *Service) beginOperation(ctx context.Context, meta OperationMeta, action, targetType, targetID string, intent operationIntent) (bool, int64, error) {
	if s.store == nil {
		return false, 0, ErrAuditUnavailable
	}
	_, outcomeKey := auditKeys(meta.IdempotencyKey)
	if _, err := s.store.GetOperationAuditByIdempotencyKey(ctx, outcomeKey); err == nil {
		replay, auditID, _, lookupErr := s.lookupOperation(ctx, meta, action, targetType, targetID, intent.Request)
		return replay, auditID, lookupErr
	} else if !errors.Is(err, toolstore.ErrNotFound) {
		return false, 0, fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return false, 0, fmt.Errorf("encode operation intent: %w", err)
	}
	intentKey, _ := auditKeys(meta.IdempotencyKey)
	claimedAudit, claimed, claimErr := s.store.ClaimOperationAudit(ctx, toolstore.OperationAuditInput{
		RequestID:      meta.RequestID,
		Actor:          meta.Actor,
		SourceIP:       meta.SourceIP,
		AuthMethod:     meta.AuthMethod,
		Action:         action + ".intent",
		TargetType:     targetType,
		TargetID:       targetID,
		Reason:         strings.TrimSpace(meta.Reason),
		BeforeJSON:     intentJSON,
		Status:         toolstore.OperationSucceeded,
		IdempotencyKey: intentKey,
		OccurredAt:     s.now().UTC(),
	})
	if claimErr == nil && claimed {
		return false, 0, nil
	}
	if claimErr != nil && !errors.Is(claimErr, toolstore.ErrConflict) {
		return false, 0, fmt.Errorf("%w: %v", ErrAuditUnavailable, claimErr)
	}

	// Another service or process already owns this intent. Re-read the full
	// chain because its outcome may have become durable between the initial
	// lookup and the atomic claim. Never continue to the upstream side effect
	// when this caller did not insert the intent.
	replay, auditID, found, lookupErr := s.lookupOperation(ctx, meta, action, targetType, targetID, intent.Request)
	if lookupErr != nil || found {
		return replay, auditID, lookupErr
	}
	return false, claimedAudit.ID, ErrOperationUncertain
}

func (s *Service) lookupOperation(ctx context.Context, meta OperationMeta, action, targetType, targetID string, request json.RawMessage) (bool, int64, bool, error) {
	if s.store == nil {
		return false, 0, false, ErrAuditUnavailable
	}
	intentKey, outcomeKey := auditKeys(meta.IdempotencyKey)
	if outcome, err := s.store.GetOperationAuditByIdempotencyKey(ctx, outcomeKey); err == nil {
		if outcome.Actor != meta.Actor || outcome.Action != action+".outcome" || outcome.TargetType != targetType || outcome.TargetID != targetID {
			return false, 0, true, ErrIdempotencyConflict
		}
		existing, intentErr := s.store.GetOperationAuditByIdempotencyKey(ctx, intentKey)
		if intentErr != nil {
			if errors.Is(intentErr, toolstore.ErrNotFound) {
				return false, outcome.ID, true, ErrOperationUncertain
			}
			return false, 0, true, fmt.Errorf("%w: %v", ErrAuditUnavailable, intentErr)
		}
		if existing.Actor != meta.Actor || existing.Action != action+".intent" || existing.TargetType != targetType ||
			existing.TargetID != targetID || !bytes.Equal(outcome.BeforeJSON, existing.BeforeJSON) ||
			!operationRequestMatches(existing.BeforeJSON, request) {
			return false, 0, true, ErrIdempotencyConflict
		}
		if outcome.Status == toolstore.OperationSucceeded {
			return true, outcome.ID, true, nil
		}
		if outcome.Status == toolstore.OperationCancelled {
			return false, outcome.ID, true, ErrOperationUncertain
		}
		return false, outcome.ID, true, ErrPreviousOperationFailed
	} else if !errors.Is(err, toolstore.ErrNotFound) {
		return false, 0, false, fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	if existing, err := s.store.GetOperationAuditByIdempotencyKey(ctx, intentKey); err == nil {
		if existing.Actor != meta.Actor || existing.Action != action+".intent" || existing.TargetType != targetType ||
			existing.TargetID != targetID || !operationRequestMatches(existing.BeforeJSON, request) {
			return false, 0, true, ErrIdempotencyConflict
		}
		return false, existing.ID, true, ErrOperationUncertain
	} else if !errors.Is(err, toolstore.ErrNotFound) {
		return false, 0, false, fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	return false, 0, false, nil
}

func (s *Service) appendOutcome(ctx context.Context, meta OperationMeta, action, targetType, targetID string, intent operationIntent, outcome any, status toolstore.OperationStatus, errorCode string) (toolstore.OperationAudit, error) {
	intentJSON, err := json.Marshal(intent)
	if err != nil {
		return toolstore.OperationAudit{}, err
	}
	outcomeJSON, err := json.Marshal(outcome)
	if err != nil {
		return toolstore.OperationAudit{}, err
	}
	_, outcomeKey := auditKeys(meta.IdempotencyKey)
	return s.store.AppendOperationAudit(ctx, toolstore.OperationAuditInput{
		RequestID:      meta.RequestID,
		Actor:          meta.Actor,
		SourceIP:       meta.SourceIP,
		AuthMethod:     meta.AuthMethod,
		Action:         action + ".outcome",
		TargetType:     targetType,
		TargetID:       targetID,
		Reason:         strings.TrimSpace(meta.Reason),
		BeforeJSON:     intentJSON,
		AfterJSON:      outcomeJSON,
		Status:         status,
		ErrorCode:      errorCode,
		IdempotencyKey: outcomeKey,
		OccurredAt:     s.now().UTC(),
	})
}

func (s *Service) finishFailure(ctx context.Context, meta OperationMeta, action, targetType, targetID string, intent operationIntent, errorCode string, status toolstore.OperationStatus, operationErr error) error {
	outcome := map[string]any{"error_code": errorCode}
	if _, err := s.appendOutcome(ctx, meta, action, targetType, targetID, intent, outcome, status, errorCode); err != nil {
		s.metrics.IncOperation(action, "audit_error")
		return ErrOperationUncertain
	}
	result := "error"
	if status == toolstore.OperationDenied {
		result = "denied"
	}
	s.metrics.IncOperation(action, result)
	return operationErr
}

func (s *Service) newOutcomeContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := s.outcomeAuditTimeout
	if timeout <= 0 {
		timeout = defaultOutcomeAuditTimeout
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

func (s *Service) redemptionOperationContexts(parent context.Context, configuredTimeout, fallbackTimeout time.Duration) (
	context.Context, context.Context, context.Context, context.CancelFunc,
) {
	timeout := configuredTimeout
	if timeout <= 0 {
		timeout = fallbackTimeout
	}
	reserve := s.redemptionAuditReserve
	if reserve <= 0 || reserve >= timeout {
		reserve = timeout / 10
	}
	deadline := time.Now().Add(timeout)
	operationCtx, operationCancel := context.WithDeadline(parent, deadline)
	auditCtx, auditCancel := context.WithDeadline(context.WithoutCancel(parent), deadline)
	upstreamCtx, upstreamCancel := context.WithDeadline(operationCtx, deadline.Add(-reserve))
	return operationCtx, auditCtx, upstreamCtx, func() {
		upstreamCancel()
		auditCancel()
		operationCancel()
	}
}

func newOperationIntent(request, before any) (operationIntent, error) {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return operationIntent{}, fmt.Errorf("encode operation request fingerprint: %w", err)
	}
	return operationIntent{Request: requestJSON, Before: before}, nil
}

func operationRequestMatches(stored json.RawMessage, expected json.RawMessage) bool {
	var intent operationIntent
	if err := json.Unmarshal(stored, &intent); err != nil || len(intent.Request) == 0 {
		return false
	}
	return bytes.Equal(intent.Request, expected)
}

func (s *Service) detectCapabilities(ctx context.Context) (newapi.Capabilities, string, error) {
	if s.upstream == nil {
		return newapi.Capabilities{}, "", errors.New("NewAPI adapter unavailable")
	}
	status, err := s.upstream.Status(ctx)
	if err != nil {
		return newapi.Capabilities{}, "", err
	}
	return newapi.DetectCapabilities(status.Version), status.Version, nil
}

func (s *Service) readUserState(ctx context.Context, userID int) (map[string]any, bool, error) {
	if s.mainDB == nil || s.mainDB.DB == nil {
		return nil, false, errors.New("main database unavailable")
	}
	query := s.mainDB.RebindQuery(`SELECT id, COALESCE(status, 0), COALESCE(role, 0),
		CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END FROM users WHERE id = ?`)
	var id, status, role, deleted int64
	err := s.mainDB.DB.QueryRowContext(ctx, query, userID).Scan(&id, &status, &role, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"exists": false}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read user audit state: %w", err)
	}
	return map[string]any{
		"exists":  true,
		"id":      id,
		"status":  status,
		"role":    role,
		"deleted": deleted == 1,
	}, true, nil
}

func validateMeta(meta OperationMeta, requireReason bool) error {
	if strings.TrimSpace(meta.RequestID) == "" || strings.TrimSpace(meta.Actor) == "" ||
		strings.TrimSpace(meta.SourceIP) == "" || strings.TrimSpace(meta.AuthMethod) == "" {
		return errors.New("request audit metadata is incomplete")
	}
	role := normalizeActorRole(meta.ActorRole)
	if role != "operator" && role != "admin" {
		return ErrOperatorRoleRequired
	}
	key := strings.TrimSpace(meta.IdempotencyKey)
	if len(key) < 8 || len(key) > 128 {
		return ErrIdempotencyRequired
	}
	for _, r := range key {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':') {
			return ErrIdempotencyRequired
		}
	}
	reason := strings.TrimSpace(meta.Reason)
	if requireReason && (len(reason) < 3 || len(reason) > 1000) {
		return ErrReasonRequired
	}
	return nil
}

func normalizeActorRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func auditKeys(key string) (string, string) {
	key = strings.TrimSpace(key)
	return "cp:" + key + ":intent", "cp:" + key + ":outcome"
}

func upstreamErrorCode(err error) string {
	if errors.Is(err, newapi.ErrAdminCredentialsMissing) {
		return "NEWAPI_ADMIN_CREDENTIALS_MISSING"
	}
	if errors.Is(err, newapi.ErrUnsupportedCapability) {
		return "NEWAPI_CAPABILITY_UNAVAILABLE"
	}
	var apiErr *newapi.APIError
	if errors.As(err, &apiErr) {
		return "NEWAPI_REJECTED"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "NEWAPI_TIMEOUT"
	}
	if isUncertainUpstreamError(err) {
		return "NEWAPI_TRANSPORT_UNCERTAIN"
	}
	return "NEWAPI_REQUEST_FAILED"
}

func isUncertainUpstreamError(err error) bool {
	if err == nil || errors.Is(err, newapi.ErrAdminCredentialsMissing) || errors.Is(err, newapi.ErrUnsupportedCapability) {
		return false
	}
	var apiErr *newapi.APIError
	return !errors.As(err, &apiErr)
}

func joinIntIDs(ids []int) string {
	parts := make([]string, len(ids))
	for index, id := range ids {
		parts[index] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}
