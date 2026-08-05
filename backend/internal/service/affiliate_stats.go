package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/new-api-tools/backend/internal/database"
)

const (
	inviteTopUpUnknownCurrency   = "XXX"
	inviteTopUpSourceUnit        = "source_native"
	inviteTopUpStateEmpty        = "empty"
	inviteTopUpStateUnreconciled = "unreconciled"
	inviteTopUpTimezone          = "Asia/Shanghai"
	inviteTopUpFingerprintV2     = "invite-topup-query-v2"

	defaultAffiliateEvidenceRowCap      int64 = 20_000
	minimumAffiliateEvidenceRowCap      int64 = 1
	maximumAffiliateEvidenceRowCap      int64 = 100_000
	defaultAffiliateQueryTimeout              = 5 * time.Second
	minimumAffiliateQueryTimeout              = time.Second
	maximumAffiliateQueryTimeout              = time.Minute
	defaultAffiliateQueryMaxConcurrency       = 2
	minimumAffiliateQueryMaxConcurrency       = 1
	maximumAffiliateQueryMaxConcurrency       = 16

	// Evidence decimals are intentionally bounded before canonicalization. The
	// limits are far above any legitimate top-up amount while preventing an
	// unconstrained NUMERIC value from turning a row-capped evidence query into
	// an unbounded allocation or exponent-parsing workload.
	maximumAffiliateEvidenceDecimalInputBytes        = 2_048
	maximumAffiliateEvidenceDecimalCoefficientDigits = 1_024
	maximumAffiliateEvidenceDecimalExponentMagnitude = int64(1_000_000)
)

var inviteTopUpLocation = time.FixedZone(inviteTopUpTimezone, 8*60*60)

var (
	ErrInvalidAffiliateStatsParams    = errors.New("invalid invite top-up analysis parameters")
	ErrAffiliateSnapshotChanged       = errors.New("invite top-up analysis snapshot changed")
	ErrAffiliateInviterNotFound       = errors.New("invite top-up analysis inviter not found")
	ErrAffiliateEvidenceScaleExceeded = errors.New("invite top-up evidence scale exceeded")
	ErrAffiliateStatsUnavailable      = errors.New("invite top-up analysis unavailable")
)

type affiliateStatsGuardrails struct {
	evidenceRowCap      int64
	queryTimeout        time.Duration
	queryMaxConcurrency int
}

var affiliateStatsGuardrailConfig atomic.Pointer[affiliateStatsGuardrails]

type affiliateQueryConcurrencyLimiter struct {
	mu      sync.Mutex
	active  int
	changed chan struct{}
}

var affiliateQueryLimiter = affiliateQueryConcurrencyLimiter{changed: make(chan struct{})}

func init() {
	affiliateStatsGuardrailConfig.Store(&affiliateStatsGuardrails{
		evidenceRowCap:      defaultAffiliateEvidenceRowCap,
		queryTimeout:        defaultAffiliateQueryTimeout,
		queryMaxConcurrency: defaultAffiliateQueryMaxConcurrency,
	})
}

// ConfigureAffiliateStatsGuardrails installs one immutable per-process
// guardrail snapshot. Production calls it once during startup, before serving
// requests. Isolated service tests use the conservative defaults above.
func ConfigureAffiliateStatsGuardrails(evidenceRowCap int64, queryTimeout time.Duration, queryMaxConcurrency int) error {
	if evidenceRowCap < minimumAffiliateEvidenceRowCap || evidenceRowCap > maximumAffiliateEvidenceRowCap {
		return fmt.Errorf("affiliate evidence row cap must be between %d and %d", minimumAffiliateEvidenceRowCap, maximumAffiliateEvidenceRowCap)
	}
	if queryTimeout < minimumAffiliateQueryTimeout || queryTimeout > maximumAffiliateQueryTimeout {
		return fmt.Errorf("affiliate query timeout must be between %s and %s", minimumAffiliateQueryTimeout, maximumAffiliateQueryTimeout)
	}
	if queryMaxConcurrency < minimumAffiliateQueryMaxConcurrency || queryMaxConcurrency > maximumAffiliateQueryMaxConcurrency {
		return fmt.Errorf("affiliate query max concurrency must be between %d and %d", minimumAffiliateQueryMaxConcurrency, maximumAffiliateQueryMaxConcurrency)
	}
	affiliateStatsGuardrailConfig.Store(&affiliateStatsGuardrails{
		evidenceRowCap:      evidenceRowCap,
		queryTimeout:        queryTimeout,
		queryMaxConcurrency: queryMaxConcurrency,
	})
	return nil
}

func currentAffiliateStatsGuardrails() affiliateStatsGuardrails {
	configured := affiliateStatsGuardrailConfig.Load()
	if configured == nil {
		return affiliateStatsGuardrails{
			evidenceRowCap:      defaultAffiliateEvidenceRowCap,
			queryTimeout:        defaultAffiliateQueryTimeout,
			queryMaxConcurrency: defaultAffiliateQueryMaxConcurrency,
		}
	}
	return *configured
}

func (l *affiliateQueryConcurrencyLimiter) acquire(ctx context.Context, limit int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		if ctx.Err() != nil {
			l.mu.Unlock()
			return ctx.Err()
		}
		if l.active < limit {
			l.active++
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (l *affiliateQueryConcurrencyLimiter) release() {
	l.mu.Lock()
	if l.active < 1 {
		l.mu.Unlock()
		panic("affiliate query concurrency limiter released without an acquisition")
	}
	l.active--
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

func (l *affiliateQueryConcurrencyLimiter) activeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

type affiliateStatsOperation struct {
	ctx       context.Context
	cancel    context.CancelFunc
	guardrail affiliateStatsGuardrails
}

func startAffiliateStatsOperation(parent context.Context) (*affiliateStatsOperation, error) {
	if parent == nil {
		parent = context.Background()
	}
	guardrail := currentAffiliateStatsGuardrails()
	ctx, cancel := context.WithTimeout(parent, guardrail.queryTimeout)
	if err := affiliateQueryLimiter.acquire(ctx, guardrail.queryMaxConcurrency); err != nil {
		cancel()
		return nil, fmt.Errorf("%w: wait for bounded query capacity: %w", ErrAffiliateStatsUnavailable, err)
	}
	return &affiliateStatsOperation{ctx: ctx, cancel: cancel, guardrail: guardrail}, nil
}

func (o *affiliateStatsOperation) close() {
	affiliateQueryLimiter.release()
	o.cancel()
}

func classifyAffiliateStatsError(err error) error {
	if err == nil ||
		errors.Is(err, ErrInvalidAffiliateStatsParams) ||
		errors.Is(err, ErrAffiliateSnapshotChanged) ||
		errors.Is(err, ErrAffiliateInviterNotFound) ||
		errors.Is(err, ErrAffiliateEvidenceScaleExceeded) ||
		errors.Is(err, ErrAffiliateStatsUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrAffiliateStatsUnavailable, err)
}

// AffiliateAnalysisMetadata describes the immutable query boundary and the
// limits of the source data. NewAPI's top_ups table has no currency or credited
// quota snapshot and users.inviter_id is current state rather than an event-time
// attribution. Monetary aggregates therefore remain null and non-empty results
// are explicitly unreconciled.
type AffiliateAnalysisMetadata struct {
	Currency            string `json:"currency"`
	Unit                string `json:"unit"`
	SourceState         string `json:"source_state"`
	AsOf                int64  `json:"as_of"`
	Timezone            string `json:"timezone"`
	SnapshotConsistency string `json:"snapshot_consistency"`
	QueryFingerprint    string `json:"query_fingerprint"`
}

// AffiliateStatsRow is one inviter in the invite top-up analysis. The three
// invite-related counts are intentionally separate and must not be presented as
// interchangeable values.
type AffiliateStatsRow struct {
	InviterID                int64    `db:"inviter_id" json:"inviter_id"`
	InviterUsername          *string  `db:"inviter_username" json:"inviter_username"`
	InviterDisplayName       *string  `db:"inviter_display_name" json:"inviter_display_name"`
	CurrentInviteeCount      int64    `db:"current_invitee_count" json:"current_invitee_count"`
	WindowPayingInviteeCount int64    `db:"window_paying_invitee_count" json:"window_paying_invitee_count"`
	RewardedInviteCount      int64    `db:"rewarded_invite_count" json:"rewarded_invite_count"`
	SuccessTopUpCount        int64    `db:"success_topup_count" json:"success_topup_count"`
	SuccessAmount            *int64   `json:"success_amount"`
	SuccessMoney             *float64 `json:"success_money"`
	LastTopUpAt              *int64   `db:"last_topup_at" json:"last_topup_at"`
	DetailEvidenceHash       string   `json:"detail_evidence_hash"`
}

// AffiliateStatsParams defines the shared parent query. AsOf is an inclusive
// second-level snapshot boundary. StartDate and EndDate are local calendar
// dates; EndDate is normalized to the next local midnight so SQL uses a half-
// open [start, end) interval.
type AffiliateStatsParams struct {
	Page                       int    `json:"page"`
	PageSize                   int    `json:"page_size"`
	Search                     string `json:"search"`
	StartDate                  string `json:"start_date"`
	EndDate                    string `json:"end_date"`
	SortBy                     string `json:"sort_by"`
	SortDir                    string `json:"sort_dir"`
	AsOf                       int64  `json:"as_of"`
	ExpectedFingerprint        string `json:"query_fingerprint,omitempty"`
	ExpectedDetailTotal        *int64 `json:"expected_detail_total,omitempty"`
	ExpectedDetailEvidenceHash string `json:"expected_detail_evidence_hash,omitempty"`
}

type AffiliateStatsSummary struct {
	AffiliateAnalysisMetadata
	WindowActiveInviterCount int64    `db:"window_active_inviter_count" json:"window_active_inviter_count"`
	CurrentInviteeCount      int64    `db:"current_invitee_count" json:"current_invitee_count"`
	WindowPayingInviteeCount int64    `db:"window_paying_invitee_count" json:"window_paying_invitee_count"`
	RewardedInviteCount      int64    `db:"rewarded_invite_count" json:"rewarded_invite_count"`
	SuccessTopUpCount        int64    `db:"success_topup_count" json:"success_topup_count"`
	TotalAmount              *int64   `json:"total_amount"`
	TotalMoney               *float64 `json:"total_money"`
}

type PaginatedAffiliateStats struct {
	AffiliateAnalysisMetadata
	Items      []AffiliateStatsRow   `json:"items"`
	Summary    AffiliateStatsSummary `json:"summary"`
	Total      int64                 `json:"total"`
	Page       int                   `json:"page"`
	PageSize   int                   `json:"page_size"`
	TotalPages int                   `json:"total_pages"`
}

type AffiliateTopUpDetailRow struct {
	ID           int64    `db:"id" json:"id"`
	UserID       int64    `db:"user_id" json:"user_id"`
	Username     *string  `db:"username" json:"username"`
	Amount       *int64   `db:"amount" json:"amount"`
	Money        *float64 `db:"money" json:"money"`
	CompleteTime int64    `db:"complete_time" json:"complete_time"`
	Status       string   `db:"status" json:"status"`
}

type PaginatedAffiliateTopUpDetails struct {
	AffiliateAnalysisMetadata
	InviterID          int64                     `json:"inviter_id"`
	Items              []AffiliateTopUpDetailRow `json:"items"`
	Total              int64                     `json:"total"`
	Page               int                       `json:"page"`
	PageSize           int                       `json:"page_size"`
	TotalPages         int                       `json:"total_pages"`
	TotalAmount        *int64                    `json:"total_amount"`
	TotalMoney         *float64                  `json:"total_money"`
	DetailEvidenceHash string                    `json:"detail_evidence_hash"`
}

type normalizedAffiliateQuery struct {
	params       AffiliateStatsParams
	start        *int64
	endExclusive *int64
	fingerprint  string
}

func strictLocalDate(value string, endExclusive bool) (int64, error) {
	parsed, err := time.ParseInLocation("2006-01-02", value, inviteTopUpLocation)
	if err != nil {
		return 0, fmt.Errorf("%w: date must use YYYY-MM-DD", ErrInvalidAffiliateStatsParams)
	}
	if endExclusive {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return parsed.Unix(), nil
}

func appendAffiliateFingerprintField(digest hash.Hash, label, value string) {
	for _, field := range []string{label, value} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len([]byte(field))))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(field))
	}
}

// affiliateQueryFingerprintV2 is shared with the browser client. Each label
// and value is UTF-8 encoded behind an unsigned 32-bit big-endian byte length,
// which keeps delimiters and Unicode content unambiguous across Go and TS.
func affiliateQueryFingerprintV2(params AffiliateStatsParams) string {
	digest := sha256.New()
	fields := [][2]string{
		{"version", inviteTopUpFingerprintV2},
		{"search", params.Search},
		{"start_date", params.StartDate},
		{"end_date", params.EndDate},
		{"sort_by", params.SortBy},
		{"sort_dir", params.SortDir},
		{"as_of", strconv.FormatInt(params.AsOf, 10)},
		{"timezone", inviteTopUpTimezone},
	}
	for _, field := range fields {
		appendAffiliateFingerprintField(digest, field[0], field[1])
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func normalizeAffiliateParams(params AffiliateStatsParams) (normalizedAffiliateQuery, error) {
	if params.Page == 0 {
		params.Page = 1
	}
	if params.Page < 1 {
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: page must be positive", ErrInvalidAffiliateStatsParams)
	}
	if params.PageSize == 0 {
		params.PageSize = 20
	}
	if params.PageSize < 1 || params.PageSize > 100 {
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: page_size must be between 1 and 100", ErrInvalidAffiliateStatsParams)
	}

	params.Search = strings.TrimSpace(params.Search)
	params.SortBy = strings.ToLower(strings.TrimSpace(params.SortBy))
	if params.SortBy == "" {
		params.SortBy = "success_topup_count"
	}
	switch params.SortBy {
	case "success_topup_count", "window_paying_invitee_count", "last_topup_at", "current_invitee_count", "rewarded_invite_count":
	default:
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: unsupported sort_by", ErrInvalidAffiliateStatsParams)
	}
	params.SortDir = strings.ToLower(strings.TrimSpace(params.SortDir))
	if params.SortDir == "" {
		params.SortDir = "desc"
	}
	if params.SortDir != "asc" && params.SortDir != "desc" {
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: sort_dir must be asc or desc", ErrInvalidAffiliateStatsParams)
	}

	if params.AsOf == 0 {
		params.AsOf = time.Now().Unix()
	}
	if params.AsOf < 1 {
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: as_of must be positive", ErrInvalidAffiliateStatsParams)
	}

	var start, endExclusive *int64
	params.StartDate = strings.TrimSpace(params.StartDate)
	if params.StartDate != "" {
		ts, err := strictLocalDate(params.StartDate, false)
		if err != nil {
			return normalizedAffiliateQuery{}, err
		}
		start = &ts
	}
	params.EndDate = strings.TrimSpace(params.EndDate)
	if params.EndDate != "" {
		ts, err := strictLocalDate(params.EndDate, true)
		if err != nil {
			return normalizedAffiliateQuery{}, err
		}
		endExclusive = &ts
	}
	if start != nil && endExclusive != nil && *start >= *endExclusive {
		return normalizedAffiliateQuery{}, fmt.Errorf("%w: start_date must not be after end_date", ErrInvalidAffiliateStatsParams)
	}

	fingerprint := affiliateQueryFingerprintV2(params)
	if expected := strings.TrimSpace(params.ExpectedFingerprint); expected != "" {
		if len(expected) != sha256.Size*2 {
			return normalizedAffiliateQuery{}, fmt.Errorf("%w: query_fingerprint must be a SHA-256 hex digest", ErrInvalidAffiliateStatsParams)
		}
		if _, err := hex.DecodeString(expected); err != nil {
			return normalizedAffiliateQuery{}, fmt.Errorf("%w: query_fingerprint must be a SHA-256 hex digest", ErrInvalidAffiliateStatsParams)
		}
		if expected != fingerprint {
			return normalizedAffiliateQuery{}, fmt.Errorf("%w: query_fingerprint does not match the parent query", ErrInvalidAffiliateStatsParams)
		}
	}

	return normalizedAffiliateQuery{
		params: params, start: start, endExclusive: endExclusive, fingerprint: fingerprint,
	}, nil
}

func affiliateMetadata(query normalizedAffiliateQuery, total int64) AffiliateAnalysisMetadata {
	state := inviteTopUpStateUnreconciled
	if total == 0 {
		state = inviteTopUpStateEmpty
	}
	return AffiliateAnalysisMetadata{
		Currency:         inviteTopUpUnknownCurrency,
		Unit:             inviteTopUpSourceUnit,
		SourceState:      state,
		AsOf:             query.params.AsOf,
		Timezone:         inviteTopUpTimezone,
		QueryFingerprint: query.fingerprint,
	}
}

func affiliateReadTxOptions(db *database.Manager) *sql.TxOptions {
	isolation := sql.LevelRepeatableRead
	if db != nil && db.DB != nil {
		switch strings.ToLower(db.DB.DriverName()) {
		case "sqlite", "sqlite3":
			isolation = sql.LevelSerializable
		}
	}
	return &sql.TxOptions{Isolation: isolation, ReadOnly: true}
}

func escapeAffiliateLike(value string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(value)
}

func buildAffiliateAggWhere(query normalizedAffiliateQuery) (string, []interface{}, int) {
	db := database.Get()
	where := []string{
		fmt.Sprintf("(%s) = %s", topUpStatusBucketSQL("t.status"), db.Placeholder(1)),
		"t.complete_time IS NOT NULL",
		"t.complete_time > 0",
		"u.inviter_id IS NOT NULL",
		"u.inviter_id > 0",
	}
	args := []interface{}{"success"}
	argIdx := 2

	where = append(where, fmt.Sprintf("t.complete_time <= %s", db.Placeholder(argIdx)))
	args = append(args, query.params.AsOf)
	argIdx++
	if query.start != nil {
		where = append(where, fmt.Sprintf("t.complete_time >= %s", db.Placeholder(argIdx)))
		args = append(args, *query.start)
		argIdx++
	}
	if query.endExclusive != nil {
		where = append(where, fmt.Sprintf("t.complete_time < %s", db.Placeholder(argIdx)))
		args = append(args, *query.endExclusive)
		argIdx++
	}

	return strings.Join(where, " AND "), args, argIdx
}

func writeAffiliateEvidenceInt64(digest hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = digest.Write(encoded[:])
}

func writeAffiliateEvidenceString(digest hash.Hash, value sql.NullString) {
	if !value.Valid {
		_, _ = digest.Write([]byte{0})
		return
	}
	_, _ = digest.Write([]byte{1})
	writeAffiliateEvidenceInt64(digest, int64(len(value.String)))
	_, _ = digest.Write([]byte(value.String))
}

func writeAffiliateEvidenceNullableInt64(digest hash.Hash, value sql.NullInt64) {
	if !value.Valid {
		_, _ = digest.Write([]byte{0})
		return
	}
	_, _ = digest.Write([]byte{1})
	writeAffiliateEvidenceInt64(digest, value.Int64)
}

// affiliateEvidenceDecimal preserves exact database NUMERIC/DECIMAL values in
// evidence hashes. The public response continues to expose money as *float64;
// this scanner is used only by the parent/detail evidence contract, before a
// detail page is scanned into the response type.
type affiliateEvidenceDecimal struct {
	Canonical string
	Valid     bool
}

// Scan accepts the database/sql driver forms used by the supported engines.
// PostgreSQL NUMERIC arrives from pgx as an exact string, while SQLite/MySQL
// may supply a float64 or textual bytes depending on the declared column type.
func (value *affiliateEvidenceDecimal) Scan(source any) error {
	if value == nil {
		return errors.New("scan affiliate evidence decimal into nil receiver")
	}
	*value = affiliateEvidenceDecimal{}

	var text string
	switch source := source.(type) {
	case nil:
		return nil
	case string:
		if len(source) > maximumAffiliateEvidenceDecimalInputBytes {
			return fmt.Errorf("affiliate evidence decimal input exceeds %d bytes", maximumAffiliateEvidenceDecimalInputBytes)
		}
		text = source
	case []byte:
		if len(source) > maximumAffiliateEvidenceDecimalInputBytes {
			return fmt.Errorf("affiliate evidence decimal input exceeds %d bytes", maximumAffiliateEvidenceDecimalInputBytes)
		}
		text = string(source)
	case int64:
		text = strconv.FormatInt(source, 10)
	case float64:
		if math.IsNaN(source) || math.IsInf(source, 0) {
			return errors.New("affiliate evidence decimal is not finite")
		}
		text = strconv.FormatFloat(source, 'g', -1, 64)
	default:
		return fmt.Errorf("unsupported affiliate evidence decimal driver type %T", source)
	}

	canonical, err := canonicalizeAffiliateEvidenceDecimal(text)
	if err != nil {
		return err
	}
	value.Canonical = canonical
	value.Valid = true
	return nil
}

func canonicalizeAffiliateEvidenceDecimal(text string) (string, error) {
	if len(text) == 0 {
		return "", errors.New("affiliate evidence decimal is empty")
	}
	if len(text) > maximumAffiliateEvidenceDecimalInputBytes {
		return "", fmt.Errorf("affiliate evidence decimal input exceeds %d bytes", maximumAffiliateEvidenceDecimalInputBytes)
	}

	index := 0
	negative := false
	if text[index] == '+' || text[index] == '-' {
		negative = text[index] == '-'
		index++
		if index == len(text) {
			return "", errors.New("affiliate evidence decimal has no digits")
		}
	}

	digits := make([]byte, 0, len(text))
	fractionDigits := 0
	seenDigit := false
	seenDecimalPoint := false
	for index < len(text) && text[index] != 'e' && text[index] != 'E' {
		switch character := text[index]; {
		case character >= '0' && character <= '9':
			digits = append(digits, character)
			seenDigit = true
			if seenDecimalPoint {
				fractionDigits++
			}
		case character == '.' && !seenDecimalPoint:
			seenDecimalPoint = true
		default:
			return "", errors.New("affiliate evidence decimal contains invalid syntax")
		}
		index++
	}
	if !seenDigit {
		return "", errors.New("affiliate evidence decimal has no digits")
	}

	var explicitExponent int64
	if index < len(text) {
		index++
		exponentNegative := false
		if index < len(text) && (text[index] == '+' || text[index] == '-') {
			exponentNegative = text[index] == '-'
			index++
		}
		if index == len(text) {
			return "", errors.New("affiliate evidence decimal exponent has no digits")
		}
		for ; index < len(text); index++ {
			character := text[index]
			if character < '0' || character > '9' {
				return "", errors.New("affiliate evidence decimal exponent contains invalid syntax")
			}
			digit := int64(character - '0')
			if explicitExponent > (maximumAffiliateEvidenceDecimalExponentMagnitude-digit)/10 {
				return "", fmt.Errorf("affiliate evidence decimal exponent exceeds magnitude %d", maximumAffiliateEvidenceDecimalExponentMagnitude)
			}
			explicitExponent = explicitExponent*10 + digit
		}
		if exponentNegative {
			explicitExponent = -explicitExponent
		}
	}

	firstNonZero := 0
	for firstNonZero < len(digits) && digits[firstNonZero] == '0' {
		firstNonZero++
	}
	if firstNonZero == len(digits) {
		return "0", nil
	}
	digits = digits[firstNonZero:]

	trailingZeroes := 0
	for trailingZeroes < len(digits) && digits[len(digits)-1-trailingZeroes] == '0' {
		trailingZeroes++
	}
	coefficient := digits[:len(digits)-trailingZeroes]
	if len(coefficient) > maximumAffiliateEvidenceDecimalCoefficientDigits {
		return "", fmt.Errorf("affiliate evidence decimal coefficient exceeds %d digits", maximumAffiliateEvidenceDecimalCoefficientDigits)
	}

	exponent := explicitExponent - int64(fractionDigits) + int64(trailingZeroes)
	if exponent < -maximumAffiliateEvidenceDecimalExponentMagnitude || exponent > maximumAffiliateEvidenceDecimalExponentMagnitude {
		return "", fmt.Errorf("affiliate evidence decimal exponent exceeds magnitude %d", maximumAffiliateEvidenceDecimalExponentMagnitude)
	}

	canonicalLength := len(coefficient) + 1 + len(strconv.FormatInt(exponent, 10))
	if negative {
		canonicalLength++
	}
	canonical := make([]byte, 0, canonicalLength)
	if negative {
		canonical = append(canonical, '-')
	}
	canonical = append(canonical, coefficient...)
	canonical = append(canonical, 'e')
	canonical = strconv.AppendInt(canonical, exponent, 10)
	return string(canonical), nil
}

func writeAffiliateEvidenceDecimal(digest hash.Hash, value affiliateEvidenceDecimal) {
	if !value.Valid {
		_, _ = digest.Write([]byte{0})
		return
	}
	_, _ = digest.Write([]byte{1})
	writeAffiliateEvidenceInt64(digest, int64(len(value.Canonical)))
	_, _ = digest.Write([]byte(value.Canonical))
}

// affiliateDetailEvidenceHashes computes a cryptographic digest over every
// field exposed by the detail API. It runs inside the caller's read
// transaction and batches all inviters on the current parent page, avoiding an
// N+1 query while detecting same-count row replacement or mutation.
func affiliateDetailEvidenceHashes(
	ctx context.Context,
	tx *sqlx.Tx,
	query normalizedAffiliateQuery,
	inviterIDs []int64,
	rowCap int64,
	expectedRows int64,
) (map[int64]string, error) {
	result := make(map[int64]string, len(inviterIDs))
	if len(inviterIDs) == 0 {
		if expectedRows != 0 {
			return nil, fmt.Errorf("invite top-up detail evidence expected %d rows without an inviter", expectedRows)
		}
		return result, nil
	}
	if rowCap < 1 || rowCap == math.MaxInt64 {
		return nil, fmt.Errorf("invalid invite top-up detail evidence row cap")
	}
	if expectedRows < 0 {
		return nil, fmt.Errorf("invalid negative invite top-up detail evidence row count")
	}
	db := database.Get()
	where, args, nextIdx := buildAffiliateAggWhere(query)
	seen := make(map[int64]struct{}, len(inviterIDs))
	placeholders := make([]string, 0, len(inviterIDs))
	orderedIDs := make([]int64, 0, len(inviterIDs))
	for _, inviterID := range inviterIDs {
		if inviterID < 1 {
			return nil, fmt.Errorf("invalid inviter id in detail evidence set")
		}
		if _, exists := seen[inviterID]; exists {
			continue
		}
		seen[inviterID] = struct{}{}
		orderedIDs = append(orderedIDs, inviterID)
		placeholders = append(placeholders, db.Placeholder(nextIdx))
		args = append(args, inviterID)
		nextIdx++
	}
	where += " AND u.inviter_id IN (" + strings.Join(placeholders, ",") + ")"
	limitPlaceholder := db.Placeholder(nextIdx)
	args = append(args, rowCap+1)
	rows, err := tx.QueryxContext(ctx, fmt.Sprintf(`SELECT u.inviter_id, t.id, t.user_id,
			u.username, t.amount, t.money, t.complete_time, COALESCE(t.status, '')
		FROM top_ups t
		JOIN users u ON u.id = t.user_id
		WHERE %s
		ORDER BY u.inviter_id ASC, t.complete_time DESC, t.id DESC
		LIMIT %s`, where, limitPlaceholder), args...)
	if err != nil {
		return nil, fmt.Errorf("query invite top-up detail evidence: %w", err)
	}
	digests := make(map[int64]hash.Hash, len(orderedIDs))
	for _, inviterID := range orderedIDs {
		digests[inviterID] = sha256.New()
	}
	var rowsRead int64
	for rows.Next() {
		rowsRead++
		if rowsRead > rowCap {
			_ = rows.Close()
			return nil, fmt.Errorf("%w: successful top-up evidence exceeds the configured %d-row cap", ErrAffiliateEvidenceScaleExceeded, rowCap)
		}
		var inviterID, id, userID, completeTime int64
		var username, status sql.NullString
		var amount sql.NullInt64
		var money affiliateEvidenceDecimal
		if err := rows.Scan(&inviterID, &id, &userID, &username, &amount, &money, &completeTime, &status); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan invite top-up detail evidence: %w", err)
		}
		digest := digests[inviterID]
		if digest == nil {
			_ = rows.Close()
			return nil, fmt.Errorf("invite top-up detail evidence returned an unexpected inviter")
		}
		writeAffiliateEvidenceInt64(digest, id)
		writeAffiliateEvidenceInt64(digest, userID)
		writeAffiliateEvidenceString(digest, username)
		writeAffiliateEvidenceNullableInt64(digest, amount)
		writeAffiliateEvidenceDecimal(digest, money)
		writeAffiliateEvidenceInt64(digest, completeTime)
		writeAffiliateEvidenceString(digest, status)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate invite top-up detail evidence: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close invite top-up detail evidence: %w", err)
	}
	if rowsRead != expectedRows {
		return nil, fmt.Errorf("invite top-up detail evidence row count changed inside the snapshot: expected %d, read %d", expectedRows, rowsRead)
	}
	for inviterID, digest := range digests {
		result[inviterID] = hex.EncodeToString(digest.Sum(nil))
	}
	return result, nil
}

func buildAffiliateSearchWhere(alias string, search string, argIdx int) (string, []interface{}, int) {
	if search == "" {
		return "1=1", nil, argIdx
	}
	db := database.Get()
	pattern := "%" + escapeAffiliateLike(search) + "%"
	condition := fmt.Sprintf(
		"(LOWER(COALESCE(%s.username,'')) LIKE LOWER(%s) ESCAPE '!' OR LOWER(COALESCE(%s.display_name,'')) LIKE LOWER(%s) ESCAPE '!')",
		alias, db.Placeholder(argIdx), alias, db.Placeholder(argIdx+1),
	)
	return condition, []interface{}{pattern, pattern}, argIdx + 2
}

func affiliateSortColumn(sortBy string) string {
	switch sortBy {
	case "window_paying_invitee_count":
		return "a.window_paying_invitee_count"
	case "last_topup_at":
		return "a.last_topup_at"
	case "current_invitee_count":
		return "COALESCE(ci.current_invitee_count, 0)"
	case "rewarded_invite_count":
		return "COALESCE(iu.aff_count, 0)"
	default:
		return "a.success_topup_count"
	}
}

func affiliateSortDir(dir string) string {
	if dir == "asc" {
		return "ASC"
	}
	return "DESC"
}

func affiliateAggregateSQL(where string) string {
	return fmt.Sprintf(`
		SELECT u.inviter_id AS inviter_id,
		       COUNT(DISTINCT t.user_id) AS window_paying_invitee_count,
		       COUNT(t.id)               AS success_topup_count,
		       MAX(t.complete_time)       AS last_topup_at
		FROM top_ups t
		JOIN users u ON u.id = t.user_id
		WHERE %s
		GROUP BY u.inviter_id
	`, where)
}

// buildAffiliateParentAggregate pushes a non-empty inviter search into the
// aggregate's user-id predicate. Filtering only after grouping would aggregate
// every inviter three times (count, summary, list) even when the operator asks
// for one exact inviter, which makes at-cap evidence requests needlessly slow.
func buildAffiliateParentAggregate(query normalizedAffiliateQuery) (string, []interface{}, int) {
	where, args, nextIdx := buildAffiliateAggWhere(query)
	if query.params.Search != "" {
		searchWhere, searchArgs, followingIdx := buildAffiliateSearchWhere("affiliate_search", query.params.Search, nextIdx)
		where += fmt.Sprintf(` AND u.inviter_id IN (
			SELECT affiliate_search.id
			FROM users affiliate_search
			WHERE affiliate_search.deleted_at IS NULL AND %s
		)`, searchWhere)
		args = append(args, searchArgs...)
		nextIdx = followingIdx
	}
	return affiliateAggregateSQL(where), args, nextIdx
}

func affiliateSummarySQL(aggregateSQL, searchWhere string) string {
	return fmt.Sprintf(`
		SELECT COUNT(*) AS window_active_inviter_count,
		       COALESCE(SUM(COALESCE(ci.current_invitee_count, 0)), 0) AS current_invitee_count,
		       COALESCE(SUM(a.window_paying_invitee_count), 0) AS window_paying_invitee_count,
		       COALESCE(SUM(COALESCE(iu.aff_count, 0)), 0) AS rewarded_invite_count,
		       COALESCE(SUM(a.success_topup_count), 0) AS success_topup_count
		FROM (%s) a
		LEFT JOIN users iu ON iu.id = a.inviter_id AND iu.deleted_at IS NULL
		LEFT JOIN (%s) ci ON ci.inviter_id = a.inviter_id
		WHERE %s`, aggregateSQL, affiliateCurrentInviteeSQL, searchWhere)
}

const affiliateCurrentInviteeSQL = `
	SELECT inviter_id, COUNT(*) AS current_invitee_count
	FROM users
	WHERE inviter_id IS NOT NULL AND inviter_id > 0 AND deleted_at IS NULL
	GROUP BY inviter_id`

func preflightAffiliateEvidenceRowCount(rowCount, rowCap int64) error {
	if rowCount < 0 {
		return fmt.Errorf("invalid negative invite top-up evidence row count")
	}
	if rowCount > rowCap {
		return fmt.Errorf("%w: %d successful top-up rows exceed the configured %d-row cap", ErrAffiliateEvidenceScaleExceeded, rowCount, rowCap)
	}
	return nil
}

func preflightAffiliatePageEvidenceRows(items []AffiliateStatsRow, rowCap int64) (int64, error) {
	var total int64
	for _, item := range items {
		if item.SuccessTopUpCount < 0 {
			return 0, fmt.Errorf("invalid negative invite top-up evidence row count for inviter %d", item.InviterID)
		}
		if item.SuccessTopUpCount > math.MaxInt64-total {
			return 0, fmt.Errorf("%w: successful top-up evidence row count overflow", ErrAffiliateEvidenceScaleExceeded)
		}
		total += item.SuccessTopUpCount
		if err := preflightAffiliateEvidenceRowCount(total, rowCap); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// ListAffiliateStats returns one page of the invite top-up analysis. Count and
// rows share one read transaction; any scan or rows iteration error makes the
// whole response unavailable rather than silently returning a partial page.
func ListAffiliateStats(params AffiliateStatsParams) (*PaginatedAffiliateStats, error) {
	return ListAffiliateStatsContext(context.Background(), params)
}

// ListAffiliateStatsContext is the request-bound variant used by HTTP
// handlers so cancellation and deadlines stop database work promptly.
func ListAffiliateStatsContext(ctx context.Context, params AffiliateStatsParams) (result *PaginatedAffiliateStats, err error) {
	query, err := normalizeAffiliateParams(params)
	if err != nil {
		return nil, err
	}
	operation, err := startAffiliateStatsOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer operation.close()
	defer func() { err = classifyAffiliateStatsError(err) }()
	ctx = operation.ctx

	db := database.Get()
	aggSQL, aggArgs, nextIdx := buildAffiliateParentAggregate(query)
	const searchWhere = "1=1"

	tx, err := db.DB.BeginTxx(ctx, affiliateReadTxOptions(db))
	if err != nil {
		return nil, fmt.Errorf("begin invite top-up analysis snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	countSQL := fmt.Sprintf(`
		SELECT COUNT(*) FROM (%s) a
		LEFT JOIN users iu ON iu.id = a.inviter_id AND iu.deleted_at IS NULL
		WHERE %s`, aggSQL, searchWhere)
	countArgs := append([]interface{}{}, aggArgs...)
	var total int64
	if err := tx.GetContext(ctx, &total, countSQL, countArgs...); err != nil {
		return nil, fmt.Errorf("count invite top-up analysis: %w", err)
	}
	var summary AffiliateStatsSummary
	if err := tx.GetContext(ctx, &summary, affiliateSummarySQL(aggSQL, searchWhere), countArgs...); err != nil {
		return nil, fmt.Errorf("query invite top-up analysis summary snapshot: %w", err)
	}

	totalPages := int((total + int64(query.params.PageSize) - 1) / int64(query.params.PageSize))
	if totalPages < 1 {
		totalPages = 1
	}
	offset := (query.params.Page - 1) * query.params.PageSize
	listSQL := fmt.Sprintf(`
		SELECT a.inviter_id,
		       iu.username AS inviter_username,
		       iu.display_name AS inviter_display_name,
		       COALESCE(ci.current_invitee_count, 0) AS current_invitee_count,
		       a.window_paying_invitee_count,
		       COALESCE(iu.aff_count, 0) AS rewarded_invite_count,
		       a.success_topup_count,
		       a.last_topup_at
		FROM (%s) a
		LEFT JOIN users iu ON iu.id = a.inviter_id AND iu.deleted_at IS NULL
		LEFT JOIN (%s) ci ON ci.inviter_id = a.inviter_id
		WHERE %s
		ORDER BY %s %s, a.inviter_id ASC
		LIMIT %s OFFSET %s`,
		aggSQL, affiliateCurrentInviteeSQL, searchWhere,
		affiliateSortColumn(query.params.SortBy), affiliateSortDir(query.params.SortDir),
		db.Placeholder(nextIdx), db.Placeholder(nextIdx+1),
	)
	listArgs := append([]interface{}{}, aggArgs...)
	listArgs = append(listArgs, query.params.PageSize, offset)
	rows, err := tx.QueryxContext(ctx, listSQL, listArgs...)
	if err != nil {
		return nil, fmt.Errorf("query invite top-up analysis: %w", err)
	}
	// Do not use the request-derived page size as a slice capacity. The SQL
	// limit is validated above, while append keeps the allocation independent
	// of untrusted input and bounded by the returned rows.
	items := make([]AffiliateStatsRow, 0)
	for rows.Next() {
		var row AffiliateStatsRow
		if err := rows.StructScan(&row); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan invite top-up analysis row: %w", err)
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate invite top-up analysis rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close invite top-up analysis rows: %w", err)
	}
	inviterIDs := make([]int64, len(items))
	for index := range items {
		inviterIDs[index] = items[index].InviterID
	}
	evidenceRows, err := preflightAffiliatePageEvidenceRows(items, operation.guardrail.evidenceRowCap)
	if err != nil {
		return nil, err
	}
	evidenceHashes, err := affiliateDetailEvidenceHashes(
		ctx, tx, query, inviterIDs, operation.guardrail.evidenceRowCap, evidenceRows,
	)
	if err != nil {
		return nil, err
	}
	for index := range items {
		items[index].DetailEvidenceHash = evidenceHashes[items[index].InviterID]
		if len(items[index].DetailEvidenceHash) != sha256.Size*2 {
			return nil, fmt.Errorf("invite top-up detail evidence is unavailable for inviter %d", items[index].InviterID)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit invite top-up analysis snapshot: %w", err)
	}

	metadata := affiliateMetadata(query, total)
	metadata.SnapshotConsistency = "repeatable_read_bundle"
	summary.AffiliateAnalysisMetadata = metadata
	return &PaginatedAffiliateStats{
		AffiliateAnalysisMetadata: metadata,
		Summary:                   summary,
		Items:                     items, Total: total, Page: query.params.Page,
		PageSize: query.params.PageSize, TotalPages: totalPages,
	}, nil
}

func GetAffiliateStatsSummary(params AffiliateStatsParams) (*AffiliateStatsSummary, error) {
	return GetAffiliateStatsSummaryContext(context.Background(), params)
}

// GetAffiliateStatsSummaryContext is the request-bound compatibility summary
// query. New clients should prefer the bundled summary from ListAffiliateStats.
func GetAffiliateStatsSummaryContext(ctx context.Context, params AffiliateStatsParams) (result *AffiliateStatsSummary, err error) {
	query, err := normalizeAffiliateParams(params)
	if err != nil {
		return nil, err
	}
	operation, err := startAffiliateStatsOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer operation.close()
	defer func() { err = classifyAffiliateStatsError(err) }()
	ctx = operation.ctx

	db := database.Get()
	aggSQL, aggArgs, _ := buildAffiliateParentAggregate(query)
	const searchWhere = "1=1"
	summarySQL := affiliateSummarySQL(aggSQL, searchWhere)
	args := append([]interface{}{}, aggArgs...)
	var summary AffiliateStatsSummary
	if err := db.DB.GetContext(ctx, &summary, summarySQL, args...); err != nil {
		return nil, fmt.Errorf("query invite top-up analysis summary: %w", err)
	}
	summary.AffiliateAnalysisMetadata = affiliateMetadata(query, summary.SuccessTopUpCount)
	summary.SnapshotConsistency = "live_unbound"
	return &summary, nil
}

// ListAffiliateTopUpDetails is the dedicated child API. It uses exactly the
// same status, complete-time, search, as_of and fingerprint contract as its
// parent, and always orders by complete_time DESC, id DESC.
func ListAffiliateTopUpDetails(inviterID int64, params AffiliateStatsParams) (*PaginatedAffiliateTopUpDetails, error) {
	return ListAffiliateTopUpDetailsContext(context.Background(), inviterID, params)
}

// ListAffiliateTopUpDetailsContext is the request-bound detail query.
func ListAffiliateTopUpDetailsContext(ctx context.Context, inviterID int64, params AffiliateStatsParams) (result *PaginatedAffiliateTopUpDetails, err error) {
	if inviterID < 1 {
		return nil, fmt.Errorf("%w: inviter_id must be positive", ErrInvalidAffiliateStatsParams)
	}
	if params.AsOf < 1 {
		return nil, fmt.Errorf("%w: as_of is required for detail queries", ErrInvalidAffiliateStatsParams)
	}
	if strings.TrimSpace(params.ExpectedFingerprint) == "" {
		return nil, fmt.Errorf("%w: query_fingerprint is required for detail queries", ErrInvalidAffiliateStatsParams)
	}
	if params.ExpectedDetailTotal == nil {
		return nil, fmt.Errorf("%w: expected_detail_total is required for detail queries", ErrInvalidAffiliateStatsParams)
	}
	expectedEvidenceHash := strings.ToLower(strings.TrimSpace(params.ExpectedDetailEvidenceHash))
	if expectedEvidenceHash == "" {
		return nil, fmt.Errorf("%w: expected_detail_evidence_hash is required", ErrInvalidAffiliateStatsParams)
	}
	if len(expectedEvidenceHash) != sha256.Size*2 {
		return nil, fmt.Errorf("%w: expected_detail_evidence_hash must be a SHA-256 hex digest", ErrInvalidAffiliateStatsParams)
	}
	if _, err := hex.DecodeString(expectedEvidenceHash); err != nil {
		return nil, fmt.Errorf("%w: expected_detail_evidence_hash must be a SHA-256 hex digest", ErrInvalidAffiliateStatsParams)
	}
	query, err := normalizeAffiliateParams(params)
	if err != nil {
		return nil, err
	}
	operation, err := startAffiliateStatsOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer operation.close()
	defer func() { err = classifyAffiliateStatsError(err) }()
	ctx = operation.ctx

	db := database.Get()
	where, args, nextIdx := buildAffiliateAggWhere(query)
	// Append the inviter predicate after the shared predicates so anonymous
	// SQLite/MySQL placeholders and numbered PostgreSQL placeholders receive the
	// same argument order.
	where += fmt.Sprintf(" AND u.inviter_id = %s", db.Placeholder(nextIdx))
	args = append(args, inviterID)
	nextIdx++
	searchWhere, searchArgs, nextIdx := buildAffiliateSearchWhere("iu", query.params.Search, nextIdx)
	where += " AND " + searchWhere
	args = append(args, searchArgs...)

	tx, err := db.DB.BeginTxx(ctx, affiliateReadTxOptions(db))
	if err != nil {
		return nil, fmt.Errorf("begin invite top-up detail snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var inviterExists int
	inviterSQL := fmt.Sprintf(
		"SELECT 1 FROM users WHERE id = %s AND deleted_at IS NULL",
		db.Placeholder(1),
	)
	if err := tx.GetContext(ctx, &inviterExists, inviterSQL, inviterID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: inviter_id %d", ErrAffiliateInviterNotFound, inviterID)
		}
		return nil, fmt.Errorf("check invite top-up analysis inviter: %w", err)
	}

	fromSQL := fmt.Sprintf(`FROM top_ups t
		JOIN users u ON u.id = t.user_id
		LEFT JOIN users iu ON iu.id = u.inviter_id AND iu.deleted_at IS NULL
		WHERE %s`, where)
	var total int64
	if err := tx.GetContext(ctx, &total, "SELECT COUNT(*) "+fromSQL, args...); err != nil {
		return nil, fmt.Errorf("count invite top-up details: %w", err)
	}
	if err := preflightAffiliateEvidenceRowCount(total, operation.guardrail.evidenceRowCap); err != nil {
		return nil, err
	}
	if query.params.ExpectedDetailTotal != nil && total != *query.params.ExpectedDetailTotal {
		return nil, fmt.Errorf("%w: expected %d detail rows, found %d", ErrAffiliateSnapshotChanged, *query.params.ExpectedDetailTotal, total)
	}
	evidenceHashes, err := affiliateDetailEvidenceHashes(
		ctx, tx, query, []int64{inviterID}, operation.guardrail.evidenceRowCap, total,
	)
	if err != nil {
		return nil, err
	}
	detailEvidenceHash := evidenceHashes[inviterID]
	if detailEvidenceHash != expectedEvidenceHash {
		return nil, fmt.Errorf("%w: detail evidence hash changed", ErrAffiliateSnapshotChanged)
	}
	totalPages := int((total + int64(query.params.PageSize) - 1) / int64(query.params.PageSize))
	if totalPages < 1 {
		totalPages = 1
	}
	offset := (query.params.Page - 1) * query.params.PageSize
	detailSQL := fmt.Sprintf(`SELECT t.id, t.user_id, u.username, t.amount, t.money,
		       t.complete_time, COALESCE(t.status, '') AS status
		%s
		ORDER BY t.complete_time DESC, t.id DESC
		LIMIT %s OFFSET %s`, fromSQL, db.Placeholder(nextIdx), db.Placeholder(nextIdx+1))
	detailArgs := append(append([]interface{}{}, args...), query.params.PageSize, offset)
	rows, err := tx.QueryxContext(ctx, detailSQL, detailArgs...)
	if err != nil {
		return nil, fmt.Errorf("query invite top-up details: %w", err)
	}
	// Keep allocation independent of the request-derived page size. The query
	// limit is already validated and bounds the number of rows appended.
	items := make([]AffiliateTopUpDetailRow, 0)
	for rows.Next() {
		var item AffiliateTopUpDetailRow
		if err := rows.StructScan(&item); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan invite top-up detail row: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate invite top-up detail rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close invite top-up detail rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit invite top-up detail snapshot: %w", err)
	}

	metadata := affiliateMetadata(query, total)
	metadata.SnapshotConsistency = "parent_evidence_bound"
	return &PaginatedAffiliateTopUpDetails{
		AffiliateAnalysisMetadata: metadata,
		InviterID:                 inviterID, Items: items, Total: total,
		Page: query.params.Page, PageSize: query.params.PageSize, TotalPages: totalPages,
		DetailEvidenceHash: detailEvidenceHash,
	}, nil
}
