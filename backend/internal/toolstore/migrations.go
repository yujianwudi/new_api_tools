package toolstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type migration struct {
	version    int
	name       string
	statements []string
}

var migrations = []migration{
	{
		version: 1,
		name:    "operation audit",
		statements: []string{
			`CREATE TABLE operation_audit (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				request_id TEXT NOT NULL CHECK(length(trim(request_id)) > 0),
				actor TEXT NOT NULL CHECK(length(trim(actor)) > 0),
				source_ip TEXT NOT NULL CHECK(length(trim(source_ip)) > 0),
				auth_method TEXT NOT NULL CHECK(length(trim(auth_method)) > 0),
				action TEXT NOT NULL CHECK(length(trim(action)) > 0),
				target_type TEXT NOT NULL CHECK(length(trim(target_type)) > 0),
				target_id TEXT NOT NULL CHECK(length(trim(target_id)) > 0),
				reason TEXT NOT NULL DEFAULT '',
				before_json TEXT CHECK(before_json IS NULL OR json_valid(before_json)),
				after_json TEXT CHECK(after_json IS NULL OR json_valid(after_json)),
				status TEXT NOT NULL CHECK(status IN ('succeeded', 'failed', 'denied', 'cancelled')),
				error_code TEXT NOT NULL DEFAULT '',
				idempotency_key TEXT,
				occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE UNIQUE INDEX idx_operation_audit_idempotency
				ON operation_audit(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_operation_audit_request
				ON operation_audit(request_id, id DESC)`,
			`CREATE INDEX idx_operation_audit_action
				ON operation_audit(action, id DESC)`,
			`CREATE INDEX idx_operation_audit_target
				ON operation_audit(target_type, target_id, id DESC)`,
			`CREATE INDEX idx_operation_audit_actor
				ON operation_audit(actor, id DESC)`,
			`CREATE INDEX idx_operation_audit_failures
				ON operation_audit(status, id DESC) WHERE status <> 'succeeded'`,
			`CREATE TRIGGER operation_audit_no_update
				BEFORE UPDATE ON operation_audit BEGIN
					SELECT RAISE(ABORT, 'operation_audit is append-only');
				END`,
			`CREATE TRIGGER operation_audit_no_delete
				BEFORE DELETE ON operation_audit BEGIN
					SELECT RAISE(ABORT, 'operation_audit is append-only');
				END`,
		},
	},
	{
		version: 2,
		name:    "risk cases",
		statements: []string{
			`CREATE TABLE risk_cases (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				case_key TEXT NOT NULL UNIQUE CHECK(length(trim(case_key)) > 0),
				title TEXT NOT NULL CHECK(length(trim(title)) > 0),
				subject_type TEXT NOT NULL CHECK(length(trim(subject_type)) > 0),
				subject_id TEXT NOT NULL CHECK(length(trim(subject_id)) > 0),
				severity TEXT NOT NULL CHECK(severity IN ('low', 'medium', 'high', 'critical')),
				status TEXT NOT NULL CHECK(status IN ('open', 'investigating', 'mitigated', 'closed')),
				assignee TEXT NOT NULL DEFAULT '',
				summary TEXT NOT NULL DEFAULT '',
				opened_at INTEGER NOT NULL CHECK(opened_at >= 0),
				closed_at INTEGER CHECK(closed_at IS NULL OR closed_at >= opened_at),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK((status = 'closed' AND closed_at IS NOT NULL) OR (status <> 'closed' AND closed_at IS NULL))
			)`,
			`CREATE INDEX idx_risk_cases_subject
				ON risk_cases(subject_type, subject_id, id DESC)`,
			`CREATE INDEX idx_risk_cases_status
				ON risk_cases(status, severity, id DESC)`,
			`CREATE INDEX idx_risk_cases_assignee_open
				ON risk_cases(assignee, id DESC) WHERE status <> 'closed'`,
			`CREATE TABLE risk_case_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				case_id INTEGER NOT NULL REFERENCES risk_cases(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				event_type TEXT NOT NULL CHECK(length(trim(event_type)) > 0),
				actor TEXT NOT NULL CHECK(length(trim(actor)) > 0),
				details_json TEXT CHECK(details_json IS NULL OR json_valid(details_json)),
				occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE INDEX idx_risk_case_events_case
				ON risk_case_events(case_id, id DESC)`,
			`CREATE INDEX idx_risk_case_events_type
				ON risk_case_events(event_type, id DESC)`,
			`CREATE TRIGGER risk_case_events_no_update
				BEFORE UPDATE ON risk_case_events BEGIN
					SELECT RAISE(ABORT, 'risk_case_events is append-only');
				END`,
			`CREATE TRIGGER risk_case_events_no_delete
				BEFORE DELETE ON risk_case_events BEGIN
					SELECT RAISE(ABORT, 'risk_case_events is append-only');
				END`,
		},
	},
	{
		version: 3,
		name:    "support notes",
		statements: []string{
			`CREATE TABLE support_notes (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				subject_type TEXT NOT NULL CHECK(length(trim(subject_type)) > 0),
				subject_id TEXT NOT NULL CHECK(length(trim(subject_id)) > 0),
				author TEXT NOT NULL CHECK(length(trim(author)) > 0),
				body TEXT NOT NULL CHECK(length(trim(body)) > 0),
				visibility TEXT NOT NULL CHECK(visibility IN ('internal', 'customer')),
				idempotency_key TEXT,
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				deleted_at INTEGER CHECK(deleted_at IS NULL OR deleted_at >= created_at)
			)`,
			`CREATE UNIQUE INDEX idx_support_notes_idempotency
				ON support_notes(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_support_notes_subject_active
				ON support_notes(subject_type, subject_id, id DESC) WHERE deleted_at IS NULL`,
			`CREATE INDEX idx_support_notes_author
				ON support_notes(author, id DESC)`,
		},
	},
	{
		version: 4,
		name:    "price snapshots",
		statements: []string{
			`CREATE TABLE price_snapshots (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				provider TEXT NOT NULL CHECK(length(trim(provider)) > 0),
				model TEXT NOT NULL CHECK(length(trim(model)) > 0),
				operation TEXT NOT NULL CHECK(length(trim(operation)) > 0),
				component TEXT NOT NULL CHECK(length(trim(component)) > 0),
				currency TEXT NOT NULL CHECK(length(currency) = 3 AND currency = upper(currency) AND currency GLOB '[A-Z][A-Z][A-Z]'),
				unit TEXT NOT NULL CHECK(length(trim(unit)) > 0),
				unit_size INTEGER NOT NULL CHECK(unit_size > 0),
				amount_decimal TEXT NOT NULL CHECK(
					length(amount_decimal) > 0 AND
					amount_decimal NOT GLOB '*[^0-9.]*' AND
					amount_decimal NOT LIKE '.%' AND
					amount_decimal NOT LIKE '%.' AND
					amount_decimal NOT GLOB '*.*.*'
				),
				amount_minor INTEGER NOT NULL CHECK(amount_minor >= 0),
				minor_unit_scale INTEGER NOT NULL CHECK(minor_unit_scale BETWEEN 0 AND 18),
				source TEXT NOT NULL CHECK(length(trim(source)) > 0),
				metadata_json TEXT CHECK(metadata_json IS NULL OR json_valid(metadata_json)),
				idempotency_key TEXT,
				effective_at INTEGER NOT NULL CHECK(effective_at >= 0),
				expires_at INTEGER CHECK(expires_at IS NULL OR expires_at > effective_at),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE UNIQUE INDEX idx_price_snapshots_idempotency
				ON price_snapshots(idempotency_key) WHERE idempotency_key IS NOT NULL`,
			`CREATE INDEX idx_price_snapshots_lookup
				ON price_snapshots(provider, model, operation, component, effective_at DESC, id DESC)`,
			`CREATE INDEX idx_price_snapshots_active
				ON price_snapshots(provider, model, id DESC) WHERE expires_at IS NULL`,
			`CREATE TRIGGER price_snapshots_no_update
				BEFORE UPDATE ON price_snapshots BEGIN
					SELECT RAISE(ABORT, 'price_snapshots is immutable');
				END`,
			`CREATE TRIGGER price_snapshots_no_delete
				BEFORE DELETE ON price_snapshots BEGIN
					SELECT RAISE(ABORT, 'price_snapshots is immutable');
				END`,
		},
	},
	{
		version: 5,
		name:    "reconciliation runs",
		statements: []string{
			`CREATE TABLE reconciliation_runs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				run_key TEXT NOT NULL UNIQUE CHECK(length(trim(run_key)) > 0),
				kind TEXT NOT NULL CHECK(length(trim(kind)) > 0),
				status TEXT NOT NULL CHECK(status IN ('running', 'succeeded', 'failed', 'cancelled')),
				window_start INTEGER NOT NULL CHECK(window_start >= 0),
				window_end INTEGER NOT NULL CHECK(window_end > window_start),
				started_at INTEGER NOT NULL CHECK(started_at >= 0),
				finished_at INTEGER CHECK(finished_at IS NULL OR finished_at >= started_at),
				scanned_count INTEGER NOT NULL DEFAULT 0 CHECK(scanned_count >= 0),
				matched_count INTEGER NOT NULL DEFAULT 0 CHECK(matched_count >= 0),
				discrepancy_count INTEGER NOT NULL DEFAULT 0 CHECK(discrepancy_count >= 0),
				discrepancy_minor INTEGER NOT NULL DEFAULT 0,
				currency TEXT NOT NULL CHECK(length(currency) = 3 AND currency = upper(currency) AND currency GLOB '[A-Z][A-Z][A-Z]'),
				summary_json TEXT CHECK(summary_json IS NULL OR json_valid(summary_json)),
				error_code TEXT NOT NULL DEFAULT '',
				error_message TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK((status = 'running' AND finished_at IS NULL) OR (status <> 'running' AND finished_at IS NOT NULL))
			)`,
			`CREATE INDEX idx_reconciliation_runs_kind
				ON reconciliation_runs(kind, id DESC)`,
			`CREATE INDEX idx_reconciliation_runs_status
				ON reconciliation_runs(status, id DESC)`,
			`CREATE INDEX idx_reconciliation_runs_active
				ON reconciliation_runs(started_at, id DESC) WHERE status = 'running'`,
		},
	},
	{
		version: 6,
		name:    "risk event idempotency",
		statements: []string{
			`ALTER TABLE risk_case_events ADD COLUMN idempotency_key TEXT`,
			`CREATE UNIQUE INDEX idx_risk_case_events_idempotency
				ON risk_case_events(idempotency_key) WHERE idempotency_key IS NOT NULL`,
		},
	},
	{
		version: 7,
		name:    "risk transition replay metadata",
		statements: []string{
			`CREATE TABLE risk_case_transition_replays (
				idempotency_key TEXT PRIMARY KEY CHECK(length(trim(idempotency_key)) > 0),
				event_id INTEGER NOT NULL UNIQUE REFERENCES risk_case_events(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				request_fingerprint TEXT NOT NULL CHECK(
					length(request_fingerprint) = 64 AND
					request_fingerprint NOT GLOB '*[^0-9a-f]*'
				),
				result_case_json TEXT NOT NULL CHECK(json_valid(result_case_json) AND json_type(result_case_json) = 'object'),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE TRIGGER risk_case_transition_replays_no_update
				BEFORE UPDATE ON risk_case_transition_replays BEGIN
					SELECT RAISE(ABORT, 'risk_case_transition_replays is append-only');
				END`,
			`CREATE TRIGGER risk_case_transition_replays_no_delete
				BEFORE DELETE ON risk_case_transition_replays BEGIN
					SELECT RAISE(ABORT, 'risk_case_transition_replays is append-only');
				END`,
		},
	},
	{
		version: 8,
		name:    "invoice documents and events",
		statements: []string{
			`CREATE TABLE invoice_documents (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				invoice_number TEXT NOT NULL CHECK(length(trim(invoice_number)) BETWEEN 1 AND 128),
				seller_entity TEXT NOT NULL CHECK(length(trim(seller_entity)) BETWEEN 1 AND 128),
				buyer_name TEXT NOT NULL CHECK(length(trim(buyer_name)) BETWEEN 1 AND 256),
				buyer_tax_id TEXT NOT NULL DEFAULT '' CHECK(length(buyer_tax_id) <= 128),
				document_kind TEXT NOT NULL CHECK(document_kind IN ('blue', 'red')),
				related_invoice_number TEXT NOT NULL DEFAULT '' CHECK(length(related_invoice_number) <= 128),
				currency TEXT NOT NULL CHECK(length(currency) = 3 AND currency = upper(currency) AND currency GLOB '[A-Z][A-Z][A-Z]'),
				amount_minor INTEGER NOT NULL CHECK(amount_minor > 0),
				tax_amount_minor INTEGER NOT NULL DEFAULT 0 CHECK(tax_amount_minor >= 0 AND tax_amount_minor <= amount_minor),
				minor_unit_scale INTEGER NOT NULL CHECK(minor_unit_scale BETWEEN 0 AND 9),
				status TEXT NOT NULL CHECK(status IN ('issued', 'voided')),
				source TEXT NOT NULL CHECK(source IN ('manual', 'csv')),
				idempotency_key TEXT NOT NULL UNIQUE CHECK(length(trim(idempotency_key)) > 0),
				request_fingerprint TEXT NOT NULL CHECK(
					length(request_fingerprint) = 64 AND
					request_fingerprint NOT GLOB '*[^0-9a-f]*'
				),
				issued_at INTEGER NOT NULL CHECK(issued_at >= 0),
				voided_at INTEGER,
				void_reason TEXT NOT NULL DEFAULT '',
				created_by TEXT NOT NULL CHECK(length(trim(created_by)) BETWEEN 1 AND 256),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				UNIQUE(seller_entity, invoice_number),
				CHECK(issued_at <= created_at),
				CHECK(
					(document_kind = 'blue' AND related_invoice_number = '') OR
					(document_kind = 'red' AND length(trim(related_invoice_number)) > 0 AND
					 trim(related_invoice_number) COLLATE NOCASE <> trim(invoice_number) COLLATE NOCASE)
				),
				CHECK(
					(status = 'issued' AND voided_at IS NULL AND void_reason = '') OR
					(status = 'voided' AND voided_at IS NOT NULL AND voided_at >= issued_at AND length(trim(void_reason)) > 0)
				)
			)`,
			`CREATE INDEX idx_invoice_documents_issued
				ON invoice_documents(issued_at DESC, id DESC)`,
			`CREATE UNIQUE INDEX idx_invoice_documents_seller_number_nocase
				ON invoice_documents(seller_entity COLLATE NOCASE, invoice_number COLLATE NOCASE)`,
			`CREATE INDEX idx_invoice_documents_status
				ON invoice_documents(status, currency, minor_unit_scale, issued_at DESC, id DESC)`,
			`CREATE TRIGGER invoice_documents_restrict_update
				BEFORE UPDATE ON invoice_documents WHEN NOT (
					OLD.status = 'issued' AND NEW.status = 'voided' AND
					OLD.invoice_number = NEW.invoice_number AND OLD.seller_entity = NEW.seller_entity AND
					OLD.buyer_name = NEW.buyer_name AND OLD.buyer_tax_id = NEW.buyer_tax_id AND
					OLD.document_kind = NEW.document_kind AND OLD.related_invoice_number = NEW.related_invoice_number AND
					OLD.currency = NEW.currency AND OLD.amount_minor = NEW.amount_minor AND
					OLD.tax_amount_minor = NEW.tax_amount_minor AND OLD.minor_unit_scale = NEW.minor_unit_scale AND
					OLD.source = NEW.source AND OLD.idempotency_key = NEW.idempotency_key AND
					OLD.request_fingerprint = NEW.request_fingerprint AND OLD.issued_at = NEW.issued_at AND
					OLD.voided_at IS NULL AND NEW.voided_at IS NOT NULL AND
					OLD.void_reason = '' AND length(trim(NEW.void_reason)) > 0 AND
					OLD.created_by = NEW.created_by AND OLD.created_at = NEW.created_at AND
					NEW.updated_at >= OLD.updated_at
				) BEGIN
					SELECT RAISE(ABORT, 'invoice_documents only permits issued-to-voided transitions');
				END`,
			`CREATE TRIGGER invoice_documents_no_delete
				BEFORE DELETE ON invoice_documents BEGIN
					SELECT RAISE(ABORT, 'invoice_documents cannot be deleted');
				END`,
			`CREATE TABLE invoice_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				invoice_id INTEGER NOT NULL REFERENCES invoice_documents(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				event_type TEXT NOT NULL CHECK(event_type IN ('created', 'imported', 'voided')),
				actor TEXT NOT NULL CHECK(length(trim(actor)) BETWEEN 1 AND 256),
				details_json TEXT NOT NULL CHECK(json_valid(details_json) AND json_type(details_json) = 'object'),
				idempotency_key TEXT NOT NULL UNIQUE CHECK(length(trim(idempotency_key)) > 0),
				occurred_at INTEGER NOT NULL CHECK(occurred_at >= 0),
				created_at INTEGER NOT NULL CHECK(created_at >= 0)
			)`,
			`CREATE INDEX idx_invoice_events_invoice
				ON invoice_events(invoice_id, id ASC)`,
			`CREATE TRIGGER invoice_events_no_update
				BEFORE UPDATE ON invoice_events BEGIN
					SELECT RAISE(ABORT, 'invoice_events is append-only');
				END`,
			`CREATE TRIGGER invoice_events_no_delete
				BEFORE DELETE ON invoice_events BEGIN
					SELECT RAISE(ABORT, 'invoice_events is append-only');
				END`,
		},
	},
	{
		version: 9,
		name:    "active model probes",
		statements: []string{
			`CREATE TABLE model_probe_runs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				run_key TEXT NOT NULL UNIQUE CHECK(length(trim(run_key)) BETWEEN 8 AND 256),
				request_fingerprint TEXT NOT NULL CHECK(
					length(request_fingerprint) = 64 AND request_fingerprint NOT GLOB '*[^0-9a-f]*'
				),
				trigger_kind TEXT NOT NULL CHECK(trigger_kind IN ('scheduled', 'manual')),
				actor TEXT NOT NULL CHECK(length(trim(actor)) BETWEEN 1 AND 256),
				status TEXT NOT NULL CHECK(status IN ('running', 'succeeded', 'partial', 'failed', 'skipped', 'cancelled')),
				requested_count INTEGER NOT NULL CHECK(requested_count >= 0),
				attempted_count INTEGER NOT NULL DEFAULT 0 CHECK(attempted_count >= 0),
				success_count INTEGER NOT NULL DEFAULT 0 CHECK(success_count >= 0),
				failure_count INTEGER NOT NULL DEFAULT 0 CHECK(failure_count >= 0),
				skipped_count INTEGER NOT NULL DEFAULT 0 CHECK(skipped_count >= 0),
				error_code TEXT NOT NULL DEFAULT '' CHECK(length(error_code) <= 64),
				error_message TEXT NOT NULL DEFAULT '' CHECK(length(error_message) <= 512),
				started_at INTEGER NOT NULL CHECK(started_at >= 0),
				finished_at INTEGER,
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK(attempted_count = success_count + failure_count + skipped_count),
				CHECK(attempted_count <= requested_count),
				CHECK((status = 'running' AND finished_at IS NULL) OR
					(status <> 'running' AND finished_at IS NOT NULL AND finished_at >= started_at))
			)`,
			`CREATE INDEX idx_model_probe_runs_started
				ON model_probe_runs(started_at DESC, id DESC)`,
			`CREATE INDEX idx_model_probe_runs_status
				ON model_probe_runs(status, started_at DESC, id DESC)`,
			`CREATE TABLE model_probe_attempts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				run_id INTEGER NOT NULL REFERENCES model_probe_runs(id) ON UPDATE RESTRICT ON DELETE CASCADE,
				model_name TEXT NOT NULL CHECK(length(trim(model_name)) BETWEEN 1 AND 256),
				capability TEXT NOT NULL CHECK(capability IN ('chat_completions', 'responses', 'embeddings', 'rerank', 'unsupported')),
				endpoint TEXT NOT NULL CHECK(length(endpoint) <= 128),
				outcome TEXT NOT NULL CHECK(outcome IN ('success', 'failure', 'skipped')),
				protocol_success INTEGER NOT NULL CHECK(protocol_success IN (0, 1)),
				semantic_success INTEGER CHECK(semantic_success IS NULL OR semantic_success IN (0, 1)),
				http_status INTEGER NOT NULL DEFAULT 0 CHECK(http_status BETWEEN 0 AND 599),
				header_latency_ms INTEGER CHECK(header_latency_ms IS NULL OR header_latency_ms >= 0),
				first_token_latency_ms INTEGER CHECK(first_token_latency_ms IS NULL OR first_token_latency_ms >= 0),
				total_latency_ms INTEGER NOT NULL CHECK(total_latency_ms >= 0),
				error_code TEXT NOT NULL DEFAULT '' CHECK(length(error_code) <= 64),
				error_message TEXT NOT NULL DEFAULT '' CHECK(length(error_message) <= 512),
				response_sha256 TEXT NOT NULL DEFAULT '' CHECK(
					response_sha256 = '' OR (length(response_sha256) = 64 AND response_sha256 NOT GLOB '*[^0-9a-f]*')
				),
				started_at INTEGER NOT NULL CHECK(started_at >= 0),
				finished_at INTEGER NOT NULL CHECK(finished_at >= started_at),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				CHECK((outcome = 'success' AND protocol_success = 1 AND semantic_success = 1 AND error_code = '') OR outcome <> 'success'),
				CHECK((outcome = 'skipped' AND protocol_success = 0) OR outcome <> 'skipped')
			)`,
			`CREATE INDEX idx_model_probe_attempts_model_started
				ON model_probe_attempts(model_name, started_at DESC, id DESC)`,
			`CREATE INDEX idx_model_probe_attempts_run
				ON model_probe_attempts(run_id, id ASC)`,
			`CREATE INDEX idx_model_probe_attempts_started
				ON model_probe_attempts(started_at DESC, id DESC)`,
			`CREATE TRIGGER model_probe_attempts_no_update
				BEFORE UPDATE ON model_probe_attempts BEGIN
					SELECT RAISE(ABORT, 'model_probe_attempts is append-only');
				END`,
			`CREATE TABLE model_probe_rollups (
				bucket_start INTEGER NOT NULL CHECK(bucket_start >= 0),
				model_name TEXT NOT NULL CHECK(length(trim(model_name)) BETWEEN 1 AND 256),
				capability TEXT NOT NULL CHECK(capability IN ('chat_completions', 'responses', 'embeddings', 'rerank', 'unsupported')),
				attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
				success_count INTEGER NOT NULL DEFAULT 0 CHECK(success_count >= 0),
				failure_count INTEGER NOT NULL DEFAULT 0 CHECK(failure_count >= 0),
				skipped_count INTEGER NOT NULL DEFAULT 0 CHECK(skipped_count >= 0),
				header_latency_sum_ms INTEGER NOT NULL DEFAULT 0 CHECK(header_latency_sum_ms >= 0),
				header_latency_samples INTEGER NOT NULL DEFAULT 0 CHECK(header_latency_samples >= 0),
				first_token_latency_sum_ms INTEGER NOT NULL DEFAULT 0 CHECK(first_token_latency_sum_ms >= 0),
				first_token_latency_samples INTEGER NOT NULL DEFAULT 0 CHECK(first_token_latency_samples >= 0),
				total_latency_sum_ms INTEGER NOT NULL DEFAULT 0 CHECK(total_latency_sum_ms >= 0),
				last_attempt_at INTEGER NOT NULL CHECK(last_attempt_at >= bucket_start),
				PRIMARY KEY(bucket_start, model_name, capability),
				CHECK(attempt_count = success_count + failure_count + skipped_count)
			)`,
			`CREATE INDEX idx_model_probe_rollups_model_bucket
				ON model_probe_rollups(model_name, bucket_start DESC)`,
		},
	},
	{
		version: 10,
		name:    "model probe budget reservations",
		statements: []string{
			`CREATE TABLE model_probe_attempt_lifecycles (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				run_id INTEGER NOT NULL REFERENCES model_probe_runs(id) ON UPDATE RESTRICT ON DELETE CASCADE,
				attempt_index INTEGER NOT NULL CHECK(attempt_index >= 0),
				model_name TEXT NOT NULL CHECK(length(trim(model_name)) BETWEEN 1 AND 256),
				capability TEXT NOT NULL CHECK(capability IN ('chat_completions', 'responses', 'embeddings', 'rerank', 'unsupported')),
				lifecycle_state TEXT NOT NULL CHECK(lifecycle_state IN ('reserved', 'sent', 'skipped', 'settled', 'uncertain')),
				budget_day INTEGER NOT NULL CHECK(budget_day >= 0 AND budget_day % 86400000 = 0),
				reserved_at INTEGER,
				sent_at INTEGER,
				finished_at INTEGER,
				attempt_id INTEGER UNIQUE REFERENCES model_probe_attempts(id) ON UPDATE RESTRICT ON DELETE CASCADE,
				error_code TEXT NOT NULL DEFAULT '' CHECK(length(error_code) <= 64),
				error_message TEXT NOT NULL DEFAULT '' CHECK(length(error_message) <= 512),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				UNIQUE(run_id, attempt_index),
				CHECK(
					(lifecycle_state = 'reserved' AND reserved_at IS NOT NULL AND sent_at IS NULL AND finished_at IS NULL AND attempt_id IS NULL) OR
					(lifecycle_state = 'sent' AND reserved_at IS NOT NULL AND sent_at IS NOT NULL AND sent_at >= reserved_at AND finished_at IS NULL AND attempt_id IS NULL) OR
					(lifecycle_state = 'skipped' AND sent_at IS NULL AND finished_at IS NOT NULL AND (reserved_at IS NULL OR finished_at >= reserved_at)) OR
					(lifecycle_state = 'settled' AND reserved_at IS NOT NULL AND sent_at IS NOT NULL AND finished_at IS NOT NULL AND sent_at >= reserved_at AND finished_at >= sent_at AND attempt_id IS NOT NULL) OR
					(lifecycle_state = 'uncertain' AND reserved_at IS NOT NULL AND finished_at IS NOT NULL AND (sent_at IS NULL OR sent_at >= reserved_at) AND finished_at >= COALESCE(sent_at, reserved_at) AND attempt_id IS NULL)
				)
			)`,
			`CREATE INDEX idx_model_probe_attempt_lifecycles_budget
				ON model_probe_attempt_lifecycles(budget_day, lifecycle_state, id)`,
			`CREATE INDEX idx_model_probe_attempt_lifecycles_run
				ON model_probe_attempt_lifecycles(run_id, attempt_index)`,
			`CREATE INDEX idx_model_probe_attempt_lifecycles_recovery
				ON model_probe_attempt_lifecycles(lifecycle_state, run_id)`,
			`INSERT INTO model_probe_attempt_lifecycles(
				run_id, attempt_index, model_name, capability, lifecycle_state, budget_day,
				reserved_at, sent_at, finished_at, attempt_id, error_code, error_message,
				created_at, updated_at
			) SELECT
				a.run_id,
				(SELECT COUNT(*) FROM model_probe_attempts earlier
					WHERE earlier.run_id = a.run_id AND earlier.id < a.id),
				a.model_name,
				a.capability,
				CASE WHEN a.outcome = 'skipped' THEN 'skipped' ELSE 'settled' END,
				(a.started_at / 86400000) * 86400000,
				CASE WHEN a.outcome = 'skipped' THEN NULL ELSE a.started_at END,
				CASE WHEN a.outcome = 'skipped' THEN NULL ELSE a.started_at END,
				a.finished_at,
				a.id,
				a.error_code,
				a.error_message,
				a.created_at,
				a.created_at
				FROM model_probe_attempts a`,
			`WITH RECURSIVE legacy_missing(
				run_id, attempt_index, requested_count, budget_day,
				reserved_at, finished_at, created_at
			) AS (
				SELECT
					run.id,
					(SELECT COUNT(*) FROM model_probe_attempt_lifecycles existing
						WHERE existing.run_id = run.id),
					run.requested_count,
					(run.started_at / 86400000) * 86400000,
					run.started_at,
					CASE WHEN run.updated_at >= run.started_at THEN run.updated_at ELSE run.started_at END,
					run.created_at
				FROM model_probe_runs run
				WHERE run.status = 'running' AND run.requested_count > (
					SELECT COUNT(*) FROM model_probe_attempt_lifecycles existing
					WHERE existing.run_id = run.id
				)
				UNION ALL
				SELECT
					run_id, attempt_index + 1, requested_count, budget_day,
					reserved_at, finished_at, created_at
				FROM legacy_missing
				WHERE attempt_index + 1 < requested_count
			)
			INSERT INTO model_probe_attempt_lifecycles(
				run_id, attempt_index, model_name, capability, lifecycle_state, budget_day,
				reserved_at, sent_at, finished_at, attempt_id, error_code, error_message,
				created_at, updated_at
			) SELECT
				run_id,
				attempt_index,
				'__legacy_v9_uncertain_' || attempt_index,
				'chat_completions',
				'uncertain',
				budget_day,
				reserved_at,
				reserved_at,
				finished_at,
				NULL,
				'legacy_v9_unpersisted',
				'Legacy v9 running attempt may have reached the network before result persistence',
				created_at,
				finished_at
			FROM legacy_missing`,
			`CREATE TRIGGER model_probe_attempt_lifecycles_transition
				BEFORE UPDATE ON model_probe_attempt_lifecycles WHEN NOT (
					OLD.run_id = NEW.run_id AND
					OLD.attempt_index = NEW.attempt_index AND
					OLD.model_name = NEW.model_name AND
					OLD.capability = NEW.capability AND
					OLD.budget_day = NEW.budget_day AND
					OLD.reserved_at IS NEW.reserved_at AND
					OLD.created_at = NEW.created_at AND
					NEW.updated_at >= OLD.updated_at AND
					(
						(OLD.lifecycle_state = 'reserved' AND NEW.lifecycle_state = 'sent' AND
							NEW.sent_at IS NOT NULL AND NEW.sent_at >= NEW.reserved_at AND
							NEW.finished_at IS NULL AND NEW.attempt_id IS NULL) OR
						(OLD.lifecycle_state = 'reserved' AND NEW.lifecycle_state = 'skipped' AND
							NEW.sent_at IS NULL AND NEW.finished_at IS NOT NULL AND NEW.finished_at >= NEW.reserved_at AND
							NEW.attempt_id IS NULL) OR
						(OLD.lifecycle_state = 'reserved' AND NEW.lifecycle_state = 'uncertain' AND
							NEW.sent_at IS NULL AND NEW.finished_at IS NOT NULL AND NEW.finished_at >= NEW.reserved_at AND
							NEW.attempt_id IS NULL) OR
						(OLD.lifecycle_state = 'sent' AND NEW.lifecycle_state = 'settled' AND
							OLD.sent_at IS NEW.sent_at AND NEW.finished_at IS NOT NULL AND NEW.finished_at >= NEW.sent_at AND
							NEW.attempt_id IS NOT NULL) OR
						(OLD.lifecycle_state = 'sent' AND NEW.lifecycle_state = 'uncertain' AND
							OLD.sent_at IS NEW.sent_at AND NEW.finished_at IS NOT NULL AND NEW.finished_at >= NEW.sent_at AND
							NEW.attempt_id IS NULL) OR
						(OLD.lifecycle_state = 'skipped' AND NEW.lifecycle_state = 'skipped' AND
							OLD.sent_at IS NEW.sent_at AND OLD.finished_at IS NEW.finished_at AND
							OLD.attempt_id IS NULL AND NEW.attempt_id IS NOT NULL AND
							OLD.error_code = NEW.error_code AND OLD.error_message = NEW.error_message)
					)
				) BEGIN
					SELECT RAISE(ABORT, 'invalid model probe attempt lifecycle transition');
				END`,
		},
	},
	{
		version: 11,
		name:    "verified invoice red relations",
		statements: []string{
			`CREATE TABLE invoice_document_relations (
				red_invoice_id INTEGER PRIMARY KEY REFERENCES invoice_documents(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				related_invoice_id INTEGER REFERENCES invoice_documents(id) ON UPDATE RESTRICT ON DELETE RESTRICT,
				relation_state TEXT NOT NULL CHECK(relation_state IN ('verified', 'unreconciled')),
				reason_code TEXT NOT NULL DEFAULT '' CHECK(length(reason_code) <= 64),
				related_invoice_number_snapshot TEXT NOT NULL CHECK(length(trim(related_invoice_number_snapshot)) BETWEEN 1 AND 128),
				seller_entity_snapshot TEXT NOT NULL CHECK(length(trim(seller_entity_snapshot)) BETWEEN 1 AND 128),
				currency_snapshot TEXT NOT NULL CHECK(length(currency_snapshot) = 3),
				minor_unit_scale_snapshot INTEGER NOT NULL CHECK(minor_unit_scale_snapshot BETWEEN 0 AND 9),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				updated_at INTEGER NOT NULL CHECK(updated_at >= created_at),
				CHECK(
					(relation_state = 'verified' AND related_invoice_id IS NOT NULL AND reason_code = '') OR
					(relation_state = 'unreconciled' AND length(trim(reason_code)) > 0)
				),
				CHECK(related_invoice_id IS NULL OR related_invoice_id <> red_invoice_id)
			)`,
			`CREATE INDEX idx_invoice_document_relations_related
				ON invoice_document_relations(related_invoice_id, relation_state, red_invoice_id)`,
			`CREATE INDEX idx_invoice_document_relations_state
				ON invoice_document_relations(relation_state, red_invoice_id)`,
			`WITH candidates AS (
				SELECT red.id AS red_id, MIN(blue.id) AS blue_id, COUNT(blue.id) AS candidate_count
				FROM invoice_documents red
				LEFT JOIN invoice_documents blue ON blue.document_kind = 'blue'
					AND trim(blue.seller_entity) = trim(red.seller_entity) COLLATE NOCASE
					AND trim(blue.invoice_number) = trim(red.related_invoice_number) COLLATE NOCASE
					AND blue.currency = red.currency
					AND blue.minor_unit_scale = red.minor_unit_scale
				WHERE red.document_kind = 'red'
				GROUP BY red.id
			), issued_red_counts AS (
				SELECT candidate.blue_id, COUNT(*) AS issued_count
				FROM candidates candidate
				JOIN invoice_documents red ON red.id = candidate.red_id
				WHERE candidate.candidate_count = 1 AND red.status = 'issued'
				GROUP BY candidate.blue_id
			)
			INSERT INTO invoice_document_relations(
				red_invoice_id, related_invoice_id, relation_state, reason_code,
				related_invoice_number_snapshot, seller_entity_snapshot, currency_snapshot,
				minor_unit_scale_snapshot, created_at, updated_at
			) SELECT
				red.id,
				CASE WHEN candidate.candidate_count = 1 THEN candidate.blue_id ELSE NULL END,
				CASE WHEN candidate.candidate_count = 1 AND blue.status = 'issued' AND
					(red.status = 'voided' OR (COALESCE(counts.issued_count, 0) = 1 AND red.amount_minor <= blue.amount_minor))
					THEN 'verified' ELSE 'unreconciled' END,
				CASE
					WHEN candidate.candidate_count = 0 THEN 'original_not_found'
					WHEN candidate.candidate_count > 1 THEN 'original_ambiguous'
					WHEN blue.status <> 'issued' THEN 'original_not_issued'
					WHEN red.status = 'issued' AND COALESCE(counts.issued_count, 0) <> 1 THEN 'historical_cumulative_unverified'
					WHEN red.status = 'issued' AND red.amount_minor > blue.amount_minor THEN 'historical_overage'
					ELSE ''
				END,
				red.related_invoice_number,
				red.seller_entity,
				red.currency,
				red.minor_unit_scale,
				red.created_at,
				red.updated_at
			FROM invoice_documents red
			JOIN candidates candidate ON candidate.red_id = red.id
			LEFT JOIN invoice_documents blue ON blue.id = candidate.blue_id AND candidate.candidate_count = 1
			LEFT JOIN issued_red_counts counts ON counts.blue_id = candidate.blue_id
			WHERE red.document_kind = 'red'`,
			`CREATE TRIGGER invoice_document_relations_validate_identity
				BEFORE INSERT ON invoice_document_relations WHEN
					NOT EXISTS (
						SELECT 1 FROM invoice_documents red
						WHERE red.id = NEW.red_invoice_id AND red.document_kind = 'red'
							AND red.related_invoice_number = NEW.related_invoice_number_snapshot
							AND red.seller_entity = NEW.seller_entity_snapshot
							AND red.currency = NEW.currency_snapshot
							AND red.minor_unit_scale = NEW.minor_unit_scale_snapshot
					) OR (NEW.related_invoice_id IS NOT NULL AND NOT EXISTS (
						SELECT 1 FROM invoice_documents red
						JOIN invoice_documents blue ON blue.id = NEW.related_invoice_id
						WHERE red.id = NEW.red_invoice_id AND blue.document_kind = 'blue'
							AND trim(blue.invoice_number) = trim(red.related_invoice_number) COLLATE NOCASE
							AND trim(blue.seller_entity) = trim(red.seller_entity) COLLATE NOCASE
							AND blue.currency = red.currency
							AND blue.minor_unit_scale = red.minor_unit_scale
					)) BEGIN
					SELECT RAISE(ABORT, 'invalid invoice document relation identity');
				END`,
			`CREATE TRIGGER invoice_document_relations_validate_verified
				BEFORE INSERT ON invoice_document_relations WHEN NEW.relation_state = 'verified' AND (
					NOT EXISTS (
						SELECT 1 FROM invoice_documents blue
						WHERE blue.id = NEW.related_invoice_id AND blue.document_kind = 'blue' AND blue.status = 'issued'
					) OR EXISTS (
						SELECT 1 FROM invoice_documents red
						JOIN invoice_documents blue ON blue.id = NEW.related_invoice_id
						WHERE red.id = NEW.red_invoice_id AND red.status = 'issued' AND
							red.amount_minor > blue.amount_minor - COALESCE((
								SELECT SUM(existing_red.amount_minor)
								FROM invoice_document_relations existing_relation
								JOIN invoice_documents existing_red ON existing_red.id = existing_relation.red_invoice_id
								WHERE existing_relation.related_invoice_id = NEW.related_invoice_id
									AND existing_relation.relation_state = 'verified'
									AND existing_red.status = 'issued'
							), 0)
					)
				) BEGIN
					SELECT RAISE(ABORT, 'invoice red amount exceeds original invoice');
				END`,
			`CREATE TRIGGER invoice_document_relations_transition
				BEFORE UPDATE ON invoice_document_relations WHEN NOT (
					OLD.relation_state = 'verified' AND NEW.relation_state = 'unreconciled' AND
					OLD.red_invoice_id = NEW.red_invoice_id AND
					OLD.related_invoice_id IS NEW.related_invoice_id AND
					OLD.related_invoice_number_snapshot = NEW.related_invoice_number_snapshot AND
					OLD.seller_entity_snapshot = NEW.seller_entity_snapshot AND
					OLD.currency_snapshot = NEW.currency_snapshot AND
					OLD.minor_unit_scale_snapshot = NEW.minor_unit_scale_snapshot AND
					OLD.created_at = NEW.created_at AND
					length(trim(NEW.reason_code)) > 0 AND NEW.updated_at >= OLD.updated_at
				) BEGIN
					SELECT RAISE(ABORT, 'invalid invoice document relation transition');
				END`,
			`CREATE TRIGGER invoice_document_relations_no_delete
				BEFORE DELETE ON invoice_document_relations BEGIN
					SELECT RAISE(ABORT, 'invoice document relations cannot be deleted');
				END`,
		},
	},
	{
		version: 12,
		name:    "durable model status configuration",
		statements: []string{
			`CREATE TABLE model_status_config_versions (
				version INTEGER PRIMARY KEY AUTOINCREMENT,
				previous_version INTEGER REFERENCES model_status_config_versions(version) ON UPDATE RESTRICT ON DELETE RESTRICT,
				changed_key TEXT NOT NULL CHECK(changed_key IN (
					'bootstrap', 'selected_models', 'time_window', 'theme', 'refresh_interval',
					'sort_mode', 'custom_order', 'custom_groups', 'site_title'
				)),
				config_json TEXT NOT NULL CHECK(
					json_valid(config_json) AND json_type(config_json) = 'object' AND
					length(CAST(config_json AS BLOB)) BETWEEN 2 AND 65536
				),
				actor TEXT NOT NULL CHECK(length(trim(actor)) BETWEEN 1 AND 256),
				request_id TEXT NOT NULL CHECK(length(trim(request_id)) BETWEEN 1 AND 128),
				reason TEXT NOT NULL CHECK(length(trim(reason)) BETWEEN 1 AND 512),
				operation_key TEXT NOT NULL UNIQUE CHECK(length(trim(operation_key)) BETWEEN 1 AND 256),
				created_at INTEGER NOT NULL CHECK(created_at >= 0),
				CHECK(previous_version IS NULL OR previous_version < version)
			)`,
			`CREATE INDEX idx_model_status_config_versions_request
				ON model_status_config_versions(request_id, version DESC)`,
			`CREATE INDEX idx_model_status_config_versions_created
				ON model_status_config_versions(created_at DESC, version DESC)`,
			`INSERT INTO model_status_config_versions(
				previous_version, changed_key, config_json, actor, request_id, reason, operation_key, created_at
			) VALUES (
				NULL,
				'bootstrap',
				'{"selected_models":[],"time_window":"24h","theme":"daylight","refresh_interval":60,"sort_mode":"default","custom_order":[],"custom_groups":[],"site_title":""}',
				'schema-migration',
				'schema-migration-v12',
				'initialize durable model status configuration',
				'schema:model-status-config:v12',
				0
			)`,
			`CREATE TRIGGER model_status_config_versions_no_update
				BEFORE UPDATE ON model_status_config_versions BEGIN
					SELECT RAISE(ABORT, 'model status config versions are append-only');
				END`,
			`CREATE TRIGGER model_status_config_versions_no_delete
				BEFORE DELETE ON model_status_config_versions BEGIN
					SELECT RAISE(ABORT, 'model status config versions are append-only');
				END`,
		},
	},
}

const latestSchemaVersion = 12

const migrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	checksum TEXT NOT NULL DEFAULT '',
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
)`

const legacyMigrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0)
)`

// SQLite appends columns added with ALTER TABLE to the stored CREATE TABLE
// statement. Keep that historical shape recognizable so a pre-checksum v0.5
// database can be upgraded without accepting arbitrary look-alike ledgers.
const upgradedLegacyMigrationLedgerCreateStatement = `CREATE TABLE schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	applied_at INTEGER NOT NULL CHECK(applied_at >= 0),
	checksum TEXT NOT NULL DEFAULT ''
)`

var migrationLedgerProtectionStatements = []string{
	`CREATE TRIGGER schema_migrations_no_update
		BEFORE UPDATE ON schema_migrations BEGIN
			SELECT RAISE(ABORT, 'schema_migrations is append-only');
		END`,
	`CREATE TRIGGER schema_migrations_no_delete
		BEFORE DELETE ON schema_migrations BEGIN
			SELECT RAISE(ABORT, 'schema_migrations is append-only');
		END`,
}

func (s *Store) migrate(ctx context.Context) error {
	if err := validateMigrationDefinitions(); err != nil {
		return err
	}
	if err := s.bootstrapMigrationLedger(ctx); err != nil {
		return err
	}
	if err := s.ensureMigrationLedgerSchema(ctx); err != nil {
		return err
	}
	if err := s.ensureMigrationLedgerProtection(ctx); err != nil {
		return err
	}
	applied, err := s.validateMigrationLedger(ctx)
	if err != nil {
		return err
	}

	for index := applied; index < len(migrations); index++ {
		if err := s.applyMigration(ctx, migrations[index]); err != nil {
			return err
		}
	}
	finalApplied, err := s.validateMigrationLedger(ctx)
	if err != nil {
		return err
	}
	if finalApplied != len(migrations) {
		return fmt.Errorf("toolstore migration ledger incomplete after migration: applied=%d expected=%d", finalApplied, len(migrations))
	}
	if err := s.validateAppliedMigrationObjects(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Store) bootstrapMigrationLedger(ctx context.Context) error {
	var objectType string
	err := s.db.QueryRowContext(ctx,
		"SELECT type FROM sqlite_schema WHERE name = ?", "schema_migrations").Scan(&objectType)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, migrationLedgerCreateStatement); err != nil {
			return fmt.Errorf("bootstrap schema migrations: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect schema migrations object: %w", err)
	}
	if objectType != "table" {
		return fmt.Errorf("toolstore migration ledger object has type %q, want table", objectType)
	}
	return nil
}

type migrationLedgerColumn struct {
	typeName string
	notNull  bool
	primary  bool
}

type migrationLedgerExecutor interface {
	execQueryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) ensureMigrationLedgerSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration ledger upgrade: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := validateMigrationLedgerTableDefinition(ctx, tx); err != nil {
		return err
	}
	columns, err := migrationLedgerColumns(ctx, tx)
	if err != nil {
		return err
	}
	for name, expected := range map[string]migrationLedgerColumn{
		"version":    {typeName: "INTEGER", primary: true},
		"name":       {typeName: "TEXT", notNull: true},
		"applied_at": {typeName: "INTEGER", notNull: true},
	} {
		actual, ok := columns[name]
		if !ok || !strings.EqualFold(actual.typeName, expected.typeName) ||
			(expected.notNull && !actual.notNull) || (expected.primary && !actual.primary) {
			return fmt.Errorf("toolstore migration ledger schema is incompatible at column %q", name)
		}
	}
	checksum, hasChecksum := columns["checksum"]
	if hasChecksum {
		if !strings.EqualFold(checksum.typeName, "TEXT") || !checksum.notNull {
			return fmt.Errorf("toolstore migration ledger schema is incompatible at column %q", "checksum")
		}
	}

	needsBackfill := !hasChecksum
	if hasChecksum {
		var emptyChecksums int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE checksum = ''").Scan(&emptyChecksums); err != nil {
			return fmt.Errorf("inspect migration ledger checksums: %w", err)
		}
		needsBackfill = emptyChecksums > 0
	}
	if !needsBackfill {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration ledger inspection: %w", err)
		}
		return nil
	}

	if err := dropMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if !hasChecksum {
		if _, err := tx.ExecContext(ctx,
			"ALTER TABLE schema_migrations ADD COLUMN checksum TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("add migration ledger checksum: %w", err)
		}
		if err := validateMigrationLedgerTableDefinition(ctx, tx); err != nil {
			return err
		}
	}
	applied, err := validateMigrationLedgerRows(ctx, tx, true)
	if err != nil {
		return err
	}
	for index := 0; index < applied; index++ {
		item := migrations[index]
		result, err := tx.ExecContext(ctx,
			"UPDATE schema_migrations SET checksum = ? WHERE version = ? AND checksum = ''",
			migrationChecksum(item), item.version)
		if err != nil {
			return fmt.Errorf("backfill migration ledger checksum at version %d: %w", item.version, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read migration ledger checksum backfill at version %d: %w", item.version, err)
		}
		if rows > 1 {
			return fmt.Errorf("toolstore migration ledger checksum backfill touched multiple rows at version %d", item.version)
		}
	}
	if _, err := validateMigrationLedgerRows(ctx, tx, false); err != nil {
		return err
	}
	if err := protectMigrationLedger(ctx, tx); err != nil {
		return err
	}
	if err := validateMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration ledger checksum backfill: %w", err)
	}
	return nil
}

func validateMigrationLedgerTableDefinition(ctx context.Context, queryer migrationLedgerExecutor) error {
	var objectType string
	var definition sql.NullString
	if err := queryer.QueryRowContext(ctx,
		"SELECT type, sql FROM sqlite_schema WHERE name = ?", "schema_migrations").
		Scan(&objectType, &definition); err != nil {
		return fmt.Errorf("inspect migration ledger definition: %w", err)
	}
	if objectType != "table" || !definition.Valid {
		return fmt.Errorf("toolstore migration ledger definition is incompatible: type=%q", objectType)
	}
	actual := normalizeSchemaSQL(definition.String)
	for _, allowed := range []string{
		migrationLedgerCreateStatement,
		legacyMigrationLedgerCreateStatement,
		upgradedLegacyMigrationLedgerCreateStatement,
	} {
		if actual == normalizeSchemaSQL(allowed) {
			return nil
		}
	}
	return errors.New("toolstore migration ledger table definition is incompatible")
}

func migrationLedgerColumns(ctx context.Context, queryer migrationLedgerExecutor) (map[string]migrationLedgerColumn, error) {
	rows, err := queryer.QueryContext(ctx, "PRAGMA table_info(schema_migrations)")
	if err != nil {
		return nil, fmt.Errorf("inspect migration ledger schema: %w", err)
	}
	defer rows.Close()

	columns := make(map[string]migrationLedgerColumn)
	for rows.Next() {
		var cid, notNull, primary int
		var name, typeName string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &defaultValue, &primary); err != nil {
			return nil, fmt.Errorf("scan migration ledger schema: %w", err)
		}
		columns[name] = migrationLedgerColumn{
			typeName: strings.TrimSpace(typeName),
			notNull:  notNull != 0,
			primary:  primary != 0,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration ledger schema: %w", err)
	}
	return columns, nil
}

func protectMigrationLedger(ctx context.Context, executor execQueryer) error {
	for _, statement := range migrationLedgerProtectionStatements {
		if _, err := executor.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("protect schema migrations: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureMigrationLedgerProtection(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration ledger protection check: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rebuild := false
	for index, name := range migrationLedgerProtectionNames() {
		var objectType string
		var definition sql.NullString
		err := tx.QueryRowContext(ctx,
			"SELECT type, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &definition)
		if errors.Is(err, sql.ErrNoRows) {
			rebuild = true
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect migration ledger protection %s: %w", name, err)
		}
		if objectType != "trigger" {
			return fmt.Errorf("migration ledger protection %s has type %q, want trigger", name, objectType)
		}
		if !definition.Valid || normalizeSchemaSQL(definition.String) != normalizeSchemaSQL(migrationLedgerProtectionStatements[index]) {
			rebuild = true
		}
	}
	if rebuild {
		if err := dropMigrationLedgerProtection(ctx, tx); err != nil {
			return err
		}
		if err := protectMigrationLedger(ctx, tx); err != nil {
			return err
		}
	}
	if err := validateMigrationLedgerProtection(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration ledger protection check: %w", err)
	}
	return nil
}

func validateMigrationLedgerProtection(ctx context.Context, queryer migrationLedgerExecutor) error {
	for index, name := range migrationLedgerProtectionNames() {
		var objectType string
		var definition sql.NullString
		if err := queryer.QueryRowContext(ctx,
			"SELECT type, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &definition); err != nil {
			return fmt.Errorf("validate migration ledger protection %s: %w", name, err)
		}
		if objectType != "trigger" || !definition.Valid ||
			normalizeSchemaSQL(definition.String) != normalizeSchemaSQL(migrationLedgerProtectionStatements[index]) {
			return fmt.Errorf("migration ledger protection %s is incompatible", name)
		}
	}
	return nil
}

func migrationLedgerProtectionNames() []string {
	return []string{"schema_migrations_no_update", "schema_migrations_no_delete"}
}

func dropMigrationLedgerProtection(ctx context.Context, executor execQueryer) error {
	for _, name := range migrationLedgerProtectionNames() {
		if _, err := executor.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+name); err != nil {
			return fmt.Errorf("temporarily remove migration ledger protection %s: %w", name, err)
		}
	}
	return nil
}

func validateMigrationDefinitions() error {
	if len(migrations) != latestSchemaVersion {
		return fmt.Errorf("toolstore migration definitions are incompatible: count=%d latest=%d", len(migrations), latestSchemaVersion)
	}
	seenNames := make(map[string]struct{}, len(migrations))
	for index, item := range migrations {
		expectedVersion := index + 1
		if item.version != expectedVersion || strings.TrimSpace(item.name) == "" {
			return fmt.Errorf("toolstore migration definitions are incompatible at version %d", expectedVersion)
		}
		if _, exists := seenNames[item.name]; exists {
			return fmt.Errorf("toolstore migration definitions contain duplicate name %q", item.name)
		}
		seenNames[item.name] = struct{}{}
	}
	return nil
}

func (s *Store) validateMigrationLedger(ctx context.Context) (int, error) {
	return validateMigrationLedgerRows(ctx, s.db, false)
}

func validateMigrationLedgerRows(ctx context.Context, queryer migrationLedgerExecutor, allowEmptyChecksum bool) (int, error) {
	rows, err := queryer.QueryContext(ctx,
		"SELECT version, name, COALESCE(checksum, '') FROM schema_migrations ORDER BY version")
	if err != nil {
		return 0, fmt.Errorf("read migration ledger: %w", err)
	}
	defer rows.Close()

	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return 0, fmt.Errorf("scan migration ledger: %w", err)
		}
		if applied >= len(migrations) {
			return 0, fmt.Errorf("toolstore migration ledger contains unsupported version %d", version)
		}
		expected := migrations[applied]
		if version != expected.version {
			return 0, fmt.Errorf("toolstore migration ledger is not a contiguous prefix: expected version %d, found %d", expected.version, version)
		}
		if name != expected.name {
			return 0, fmt.Errorf("toolstore migration ledger name mismatch at version %d: expected %q, found %q", version, expected.name, name)
		}
		expectedChecksum := migrationChecksum(expected)
		if checksum != expectedChecksum && !(allowEmptyChecksum && checksum == "") {
			return 0, fmt.Errorf("toolstore migration ledger checksum mismatch at version %d", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate migration ledger: %w", err)
	}
	return applied, nil
}

func migrationChecksum(item migration) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d\x00%s\x00", item.version, item.name)
	for _, statement := range item.statements {
		_, _ = hash.Write([]byte(statement))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (s *Store) applyMigration(ctx context.Context, item migration) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", item.version, err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingName, existingChecksum string
	err = tx.QueryRowContext(ctx,
		"SELECT name, COALESCE(checksum, '') FROM schema_migrations WHERE version = ?", item.version).
		Scan(&existingName, &existingChecksum)
	if err == nil {
		if existingName != item.name || existingChecksum != migrationChecksum(item) {
			return fmt.Errorf("toolstore migration ledger conflict at version %d", item.version)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit existing migration %d: %w", item.version, err)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check migration %d: %w", item.version, err)
	}
	for _, statement := range item.statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", item.version, item.name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		item.version, item.name, migrationChecksum(item), dbTime(s.clock())); err != nil {
		return fmt.Errorf("record migration %d: %w", item.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", item.version, err)
	}
	return nil
}

type schemaObjectDefinition struct {
	objectType string
	tableName  string
	statement  string
}

// validateAppliedMigrationObjects compares the live schema with a clean schema
// produced by the immutable migration set. This catches a dropped or replaced
// append-only trigger (and any other same-name object) even when the migration
// ledger itself still contains the expected checksums.
func (s *Store) validateAppliedMigrationObjects(ctx context.Context) error {
	expected, err := expectedMigrationObjects(ctx)
	if err != nil {
		return err
	}
	for name, want := range expected {
		var objectType, tableName string
		var statement sql.NullString
		err := s.db.QueryRowContext(ctx,
			"SELECT type, tbl_name, sql FROM sqlite_schema WHERE name = ?", name).
			Scan(&objectType, &tableName, &statement)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("toolstore schema object %q is missing", name)
		}
		if err != nil {
			return fmt.Errorf("inspect toolstore schema object %q: %w", name, err)
		}
		if objectType != want.objectType || tableName != want.tableName || !statement.Valid ||
			normalizeSchemaSQL(statement.String) != normalizeSchemaSQL(want.statement) {
			return fmt.Errorf("toolstore schema object %q is incompatible", name)
		}
	}
	return s.validateManagedTableTriggers(ctx, expected)
}

// validateManagedTableTriggers rejects triggers that are not part of the
// immutable migration set. An otherwise valid trigger can still change,
// suppress, or exfiltrate writes, so same-name object validation alone is not
// sufficient for tables owned by Tool Store.
func (s *Store) validateManagedTableTriggers(ctx context.Context, expected map[string]schemaObjectDefinition) error {
	managedTables := map[string]struct{}{
		"schema_migrations": {},
	}
	allowedTriggers := make(map[string]string)
	for name, object := range expected {
		switch object.objectType {
		case "table":
			managedTables[name] = struct{}{}
		case "trigger":
			allowedTriggers[name] = object.tableName
		}
	}
	for _, name := range migrationLedgerProtectionNames() {
		allowedTriggers[name] = "schema_migrations"
	}

	rows, err := s.db.QueryContext(ctx, `SELECT name, tbl_name
		FROM sqlite_schema WHERE type = 'trigger' ORDER BY name`)
	if err != nil {
		return fmt.Errorf("inspect toolstore triggers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, tableName string
		if err := rows.Scan(&name, &tableName); err != nil {
			return fmt.Errorf("scan toolstore trigger: %w", err)
		}
		if _, managed := managedTables[tableName]; !managed {
			continue
		}
		allowedTable, allowed := allowedTriggers[name]
		if !allowed || allowedTable != tableName {
			return fmt.Errorf("toolstore managed table %q has unexpected trigger %q", tableName, name)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate toolstore triggers: %w", err)
	}
	return nil
}

func expectedMigrationObjects(ctx context.Context) (map[string]schemaObjectDefinition, error) {
	reference, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open reference migration schema: %w", err)
	}
	reference.SetMaxOpenConns(1)
	defer reference.Close()

	tx, err := reference.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin reference migration schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range migrations {
		for _, statement := range item.statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return nil, fmt.Errorf("build reference migration schema at version %d: %w", item.version, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reference migration schema: %w", err)
	}

	rows, err := reference.QueryContext(ctx, `SELECT type, name, tbl_name, sql
		FROM sqlite_schema WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("read reference migration schema: %w", err)
	}
	defer rows.Close()
	objects := make(map[string]schemaObjectDefinition)
	for rows.Next() {
		var objectType, name, tableName, statement string
		if err := rows.Scan(&objectType, &name, &tableName, &statement); err != nil {
			return nil, fmt.Errorf("scan reference migration schema: %w", err)
		}
		objects[name] = schemaObjectDefinition{
			objectType: objectType,
			tableName:  tableName,
			statement:  statement,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reference migration schema: %w", err)
	}
	return objects, nil
}

// normalizeSchemaSQL ignores presentation-only differences while preserving
// quoted string contents, where whitespace and case can change constraints or
// trigger behavior. Older v0.5 development builds used IF NOT EXISTS; SQLite
// may retain that clause in sqlite_schema even though the resulting object is
// otherwise identical.
func normalizeSchemaSQL(statement string) string {
	statement = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(statement), ";"))
	var normalized strings.Builder
	normalized.Grow(len(statement))
	var quote byte
	for index := 0; index < len(statement); index++ {
		character := statement[index]
		if quote != 0 {
			normalized.WriteByte(character)
			if character == quote {
				if index+1 < len(statement) && statement[index+1] == quote {
					index++
					normalized.WriteByte(statement[index])
					continue
				}
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"', '`':
			quote = character
			normalized.WriteByte(character)
		case ' ', '\t', '\r', '\n', '\f':
			continue
		default:
			if character >= 'A' && character <= 'Z' {
				character += 'a' - 'A'
			}
			normalized.WriteByte(character)
		}
	}
	result := normalized.String()
	for _, replacement := range []struct{ old, new string }{
		{"createuniqueindexifnotexists", "createuniqueindex"},
		{"createindexifnotexists", "createindex"},
		{"createtriggerifnotexists", "createtrigger"},
		{"createtableifnotexists", "createtable"},
	} {
		result = strings.Replace(result, replacement.old, replacement.new, 1)
	}
	return result
}
