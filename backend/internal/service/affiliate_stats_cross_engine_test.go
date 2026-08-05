package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
)

// TestAffiliateEvidenceCrossEngineAcceptance is enabled by the protected CI
// quality job, which supplies real MySQL and PostgreSQL services. SQLite stays
// in the matrix as the canonical sidecar/test dialect. The same source rows
// must produce the same full-content evidence hash on every supported engine.
func TestAffiliateEvidenceCrossEngineAcceptance(t *testing.T) {
	if os.Getenv("AFFILIATE_CROSS_ENGINE") != "1" {
		t.Skip("set AFFILIATE_CROSS_ENGINE=1 with MySQL/PostgreSQL test DSNs")
	}
	useAffiliateStatsGuardrailsForTest(t, 3, 5*time.Second, 2)

	engines := []struct {
		name   string
		driver string
		dsn    string
		isPG   bool
	}{
		{name: "sqlite", driver: "sqlite", dsn: "file:affiliate-cross-engine?mode=memory&cache=shared"},
		{name: "mysql", driver: "mysql", dsn: os.Getenv("AFFILIATE_TEST_MYSQL_DSN")},
		{name: "postgresql", driver: "pgx", dsn: os.Getenv("AFFILIATE_TEST_POSTGRES_DSN"), isPG: true},
	}

	const targetInviterUsername = "邀请人%_!甲"
	searches := []string{"甲", "%", "_", "!", targetInviterUsername}
	hashes := make(map[string]string, len(engines))
	referenceFingerprints := make(map[string]string, len(searches))
	for _, engine := range engines {
		engine := engine
		t.Run(engine.name, func(t *testing.T) {
			if engine.dsn == "" {
				t.Fatalf("%s integration DSN is required", engine.name)
			}
			db, err := sqlx.Open(engine.driver, engine.dsn)
			if err != nil {
				t.Fatalf("open %s fixture: %v", engine.name, err)
			}
			db.SetMaxOpenConns(4)
			db.SetMaxIdleConns(4)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				_ = db.Close()
				t.Fatalf("ping %s fixture: %v", engine.name, err)
			}
			database.SetForTesting(&database.Manager{DB: db, IsPG: engine.isPG})
			t.Cleanup(func() {
				database.SetForTesting(nil)
				_ = db.Close()
			})

			installAffiliateCrossEngineSchema(t, db, engine.name)
			insertAffiliateCrossEngineFixture(t, db)

			seenFingerprints := make(map[string]string, len(searches))
			var mutationParent *PaginatedAffiliateStats
			var mutationParams AffiliateStatsParams
			var engineHash string
			for _, search := range searches {
				params := AffiliateStatsParams{
					Page: 1, PageSize: 20, Search: search,
					SortBy: "success_topup_count", SortDir: "desc", AsOf: 200,
				}
				parent, err := ListAffiliateStatsContext(ctx, params)
				if err != nil {
					t.Fatalf("%s search %q parent evidence: %v", engine.name, search, err)
				}
				if parent.Total != 1 || len(parent.Items) != 1 {
					t.Fatalf("%s search %q returned %d/%d inviters, want only the literal-match target: %+v", engine.name, search, parent.Total, len(parent.Items), parent.Items)
				}
				row := parent.Items[0]
				if row.InviterID != 1 || row.InviterUsername == nil || *row.InviterUsername != targetInviterUsername || row.SuccessTopUpCount != 3 || !validAffiliateCrossEngineDigest(row.DetailEvidenceHash) {
					t.Fatalf("%s search %q parent evidence is incomplete: %+v", engine.name, search, row)
				}
				assertAffiliateCrossEngineSummary(t, engine.name+" bundled search "+search, parent.Summary, parent.QueryFingerprint)

				standalone, err := GetAffiliateStatsSummaryContext(ctx, params)
				if err != nil {
					t.Fatalf("%s search %q standalone summary: %v", engine.name, search, err)
				}
				assertAffiliateCrossEngineSummary(t, engine.name+" standalone search "+search, *standalone, parent.QueryFingerprint)

				if !validAffiliateCrossEngineDigest(parent.QueryFingerprint) {
					t.Fatalf("%s search %q query fingerprint is invalid: %q", engine.name, search, parent.QueryFingerprint)
				}
				if priorSearch, exists := seenFingerprints[parent.QueryFingerprint]; exists {
					t.Fatalf("%s searches %q and %q produced the same query fingerprint %s", engine.name, priorSearch, search, parent.QueryFingerprint)
				}
				seenFingerprints[parent.QueryFingerprint] = search
				if engine.name == "sqlite" {
					referenceFingerprints[search] = parent.QueryFingerprint
				} else if parent.QueryFingerprint != referenceFingerprints[search] {
					t.Fatalf("%s search %q fingerprint = %s, want engine-independent %s", engine.name, search, parent.QueryFingerprint, referenceFingerprints[search])
				}

				if engineHash == "" {
					engineHash = row.DetailEvidenceHash
				} else if row.DetailEvidenceHash != engineHash {
					t.Fatalf("%s literal search %q changed evidence hash: first=%s got=%s", engine.name, search, engineHash, row.DetailEvidenceHash)
				}

				detailParams := params
				detailParams.PageSize = 2
				detailParams.ExpectedFingerprint = parent.QueryFingerprint
				detailParams.ExpectedDetailTotal = int64Pointer(3)
				detailParams.ExpectedDetailEvidenceHash = row.DetailEvidenceHash
				detail, err := ListAffiliateTopUpDetailsContext(ctx, 1, detailParams)
				if err != nil {
					t.Fatalf("%s search %q detail evidence: %v", engine.name, search, err)
				}
				if detail.Total != 3 || len(detail.Items) != 2 || detail.QueryFingerprint != parent.QueryFingerprint || detail.DetailEvidenceHash != row.DetailEvidenceHash {
					t.Fatalf("%s search %q detail evidence does not match parent: %+v", engine.name, search, detail)
				}

				if search == targetInviterUsername {
					mutationParent = parent
					mutationParams = params
				}
			}
			hashes[engine.name] = engineHash

			if engine.name == "postgresql" {
				if _, err := db.Exec(`UPDATE top_ups
					SET money = 1.2500000000000000000000000001
					WHERE id = 1`); err != nil {
					t.Fatalf("mutate PostgreSQL NUMERIC money: %v", err)
				}
				staleDetailParams := mutationParams
				staleDetailParams.PageSize = 2
				staleDetailParams.ExpectedFingerprint = mutationParent.QueryFingerprint
				staleDetailParams.ExpectedDetailTotal = int64Pointer(3)
				staleDetailParams.ExpectedDetailEvidenceHash = mutationParent.Items[0].DetailEvidenceHash
				staleDetail, err := ListAffiliateTopUpDetailsContext(ctx, 1, staleDetailParams)
				if staleDetail != nil || !errors.Is(err, ErrAffiliateSnapshotChanged) {
					t.Fatalf("PostgreSQL high-precision money mutation result/error = %#v/%v, want nil snapshot-changed", staleDetail, err)
				}
				refreshed, err := ListAffiliateStatsContext(ctx, mutationParams)
				if err != nil {
					t.Fatalf("refresh PostgreSQL NUMERIC parent evidence: %v", err)
				}
				if len(refreshed.Items) != 1 || refreshed.Items[0].DetailEvidenceHash == mutationParent.Items[0].DetailEvidenceHash {
					t.Fatalf("PostgreSQL NUMERIC mutation did not change exact evidence: before=%+v after=%+v", mutationParent.Items, refreshed.Items)
				}
			}

			insertAffiliateCrossEngineTopUp(t, db, 5, 10, 500, 5.5, "success", 105)
			scaled, err := ListAffiliateStatsContext(ctx, mutationParams)
			if scaled != nil || !errors.Is(err, ErrAffiliateEvidenceScaleExceeded) {
				t.Fatalf("%s cap+1 result/error = %#v/%v, want nil scale-exceeded", engine.name, scaled, err)
			}
		})
	}

	want := hashes["sqlite"]
	if len(want) != 64 {
		t.Fatalf("SQLite canonical evidence hash is invalid: %q", want)
	}
	for _, engine := range engines[1:] {
		if got := hashes[engine.name]; got != want {
			t.Fatalf("cross-engine evidence mismatch: sqlite=%s %s=%s", want, engine.name, got)
		}
	}
}

func validAffiliateCrossEngineDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func assertAffiliateCrossEngineSummary(
	t *testing.T,
	label string,
	summary AffiliateStatsSummary,
	wantFingerprint string,
) {
	t.Helper()
	if summary.WindowActiveInviterCount != 1 ||
		summary.CurrentInviteeCount != 2 ||
		summary.WindowPayingInviteeCount != 2 ||
		summary.RewardedInviteCount != 7 ||
		summary.SuccessTopUpCount != 3 ||
		summary.QueryFingerprint != wantFingerprint {
		t.Fatalf("%s summary does not match the target inviter contract: %+v", label, summary)
	}
}

func installAffiliateCrossEngineSchema(t *testing.T, db *sqlx.DB, engine string) {
	t.Helper()
	moneyType := "REAL"
	switch engine {
	case "mysql":
		moneyType = "DOUBLE"
	case "postgresql":
		moneyType = "NUMERIC"
	}
	statements := []string{
		"DROP TABLE IF EXISTS top_ups",
		"DROP TABLE IF EXISTS users",
		`CREATE TABLE users (
			id BIGINT PRIMARY KEY,
			username VARCHAR(191) NOT NULL,
			display_name VARCHAR(191),
			aff_count BIGINT NOT NULL DEFAULT 0,
			inviter_id BIGINT,
			deleted_at BIGINT
		)`,
		`CREATE TABLE top_ups (
			id BIGINT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			amount BIGINT,
			money ` + moneyType + `,
			status VARCHAR(32),
			create_time BIGINT,
			complete_time BIGINT
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("prepare %s affiliate schema with %q: %v", engine, statement, err)
		}
	}
}

func insertAffiliateCrossEngineFixture(t *testing.T, db *sqlx.DB) {
	t.Helper()
	userSQL := db.Rebind(`INSERT INTO users
		(id, username, display_name, aff_count, inviter_id, deleted_at)
		VALUES (?, ?, ?, ?, ?, ?)`)
	users := [][]any{
		{int64(1), "邀请人%_!甲", "展示甲", int64(7), nil, nil},
		{int64(2), "干扰邀请人乙", "干扰展示乙", int64(5), nil, nil},
		{int64(10), "受邀-A", "受邀 A", int64(0), int64(1), nil},
		{int64(11), "受邀-乙", "受邀乙", int64(0), int64(1), nil},
		{int64(20), "干扰受邀者", "干扰受邀者", int64(0), int64(2), nil},
	}
	for _, args := range users {
		if _, err := db.Exec(userSQL, args...); err != nil {
			t.Fatalf("insert cross-engine user: %v", err)
		}
	}
	insertAffiliateCrossEngineTopUp(t, db, 1, 10, 100, 1.25, "success", 100)
	insertAffiliateCrossEngineTopUp(t, db, 2, 11, nil, nil, "completed", 101)
	insertAffiliateCrossEngineTopUp(t, db, 3, 10, 300, 3.5, "1", 102)
	insertAffiliateCrossEngineTopUp(t, db, 4, 10, 400, 4.5, "pending", 103)
	insertAffiliateCrossEngineTopUp(t, db, 6, 20, 600, 6.5, "success", 104)
}

func insertAffiliateCrossEngineTopUp(
	t *testing.T,
	db *sqlx.DB,
	id, userID int64,
	amount, money any,
	status string,
	completeTime int64,
) {
	t.Helper()
	query := db.Rebind(`INSERT INTO top_ups
		(id, user_id, amount, money, status, create_time, complete_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if _, err := db.Exec(query, id, userID, amount, money, status, completeTime-1, completeTime); err != nil {
		t.Fatalf("insert cross-engine top-up %d: %v", id, err)
	}
}
