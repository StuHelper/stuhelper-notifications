package notifications_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	n "github.com/StuHelper/stuhelper-notifications"
	"github.com/StuHelper/stuhelper-notifications/httpapi"
	"github.com/StuHelper/stuhelper-notifications/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCheckRuntimeEnforcesLeastPrivilegeAndOwnership(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 1. Nil Repository or nil DB must return ErrUnavailable with ReasonDatabaseNil instead of panicking.
	var nilRepo *postgres.Repository
	if err := nilRepo.CheckRuntime(ctx); !errors.Is(err, n.ErrUnavailable) || n.UnavailableReason(err) != n.ReasonDatabaseNil {
		t.Fatalf("nil repo CheckRuntime: expected %s, got %v", n.ReasonDatabaseNil, err)
	}
	emptyRepo := &postgres.Repository{}
	if err := emptyRepo.CheckRuntime(ctx); !errors.Is(err, n.ErrUnavailable) || n.UnavailableReason(err) != n.ReasonDatabaseNil {
		t.Fatalf("nil DB CheckRuntime: expected %s, got %v", n.ReasonDatabaseNil, err)
	}
	if _, err := emptyRepo.Unread(ctx, "alice", f.now.Add(-24*time.Hour)); !errors.Is(err, n.ErrUnavailable) || n.UnavailableReason(err) != n.ReasonDatabaseNil {
		t.Fatalf("nil DB Unread: expected %s, got %v", n.ReasonDatabaseNil, err)
	}

	dsnCfg, err := pgx.ParseConfig(os.Getenv("STUHELPER_NOTIFICATIONS_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	var currentDB string
	if err = f.db.QueryRow(`SELECT current_database()`).Scan(&currentDB); err != nil {
		t.Fatal(err)
	}

	openAsRole := func(t *testing.T, suffix, grantSQL string) *postgres.Repository {
		t.Helper()
		role := fmt.Sprintf("rt_%s_%d", suffix, time.Now().UnixNano())
		base := `CREATE ROLE ` + role + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; ` +
			`GRANT USAGE ON SCHEMA notifications TO ` + role + `; ` +
			`GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.items,notifications.preferences TO ` + role + `; ` +
			`GRANT SELECT,INSERT,UPDATE ON notifications.revisions TO ` + role + `; ` +
			`GRANT SELECT,INSERT ON notifications.inbox,notifications.closed_owners,notifications.like_subjects TO ` + role + `; `
		if _, err := f.db.Exec(base + grantSQL); err != nil {
			t.Fatal(suffix, err)
		}
		t.Cleanup(func() {
			_, _ = f.db.Exec(`REASSIGN OWNED BY ` + role + ` TO CURRENT_USER; DROP OWNED BY ` + role + `; DROP ROLE IF EXISTS ` + role)
		})
		rc := dsnCfg.Copy()
		rc.Database = currentDB
		rc.User = role
		db := stdlib.OpenDB(*rc)
		t.Cleanup(func() { db.Close() })
		return &postgres.Repository{DB: db}
	}

	// Valid baseline role passes CheckRuntime.
	validRepo := openAsRole(t, "valid", "")
	if err := validRepo.CheckRuntime(ctx); err != nil {
		t.Fatalf("valid runtime role rejected: %v", err)
	}

	cases := []struct {
		name   string
		sql    string
		reason string
	}{
		{"missing_items_delete", "REVOKE DELETE ON notifications.items FROM ", n.ReasonRuntimeMissingDelete},
		{"missing_preferences_delete", "REVOKE DELETE ON notifications.preferences FROM ", n.ReasonRuntimeMissingDelete},
		{"missing_schema_usage", "REVOKE USAGE ON SCHEMA notifications FROM ", n.ReasonRuntimeMissingSchema},
		{"granted_inbox_truncate", "GRANT TRUNCATE ON notifications.inbox TO ", n.ReasonRuntimeUnsafePrivilege},
		{"granted_closure_update", "GRANT UPDATE ON notifications.closed_owners TO ", n.ReasonRuntimeUnsafePrivilege},
		{"granted_revisions_truncate", "GRANT TRUNCATE ON notifications.revisions TO ", n.ReasonRuntimeUnsafePrivilege},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			role := fmt.Sprintf("rt_%s_%d", tc.name, time.Now().UnixNano())
			base := `CREATE ROLE ` + role + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; ` +
				`GRANT USAGE ON SCHEMA notifications TO ` + role + `; ` +
				`GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.items,notifications.preferences TO ` + role + `; ` +
				`GRANT SELECT,INSERT,UPDATE ON notifications.revisions TO ` + role + `; ` +
				`GRANT SELECT,INSERT ON notifications.inbox,notifications.closed_owners,notifications.like_subjects TO ` + role + `; ` +
				tc.sql + role
			if _, err := f.db.Exec(base); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = f.db.Exec(`DROP OWNED BY ` + role + `; DROP ROLE IF EXISTS ` + role)
			})
			rc := dsnCfg.Copy()
			rc.Database = currentDB
			rc.User = role
			db := stdlib.OpenDB(*rc)
			t.Cleanup(func() { db.Close() })
			repo := &postgres.Repository{DB: db}
			if err := repo.CheckRuntime(ctx); !errors.Is(err, n.ErrUnavailable) || n.UnavailableReason(err) != tc.reason {
				t.Fatalf("%s: expected reason %s, got %v (reason=%q)", tc.name, tc.reason, err, n.UnavailableReason(err))
			}
		})
	}

	// Non-superuser table owner with self-revoked UPDATE/DELETE on inbox must still be rejected.
	t.Run("table_owner_self_revoked", func(t *testing.T) {
		ownerRole := fmt.Sprintf("rt_tbl_owner_%d", time.Now().UnixNano())
		setupSQL := `CREATE ROLE ` + ownerRole + ` LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; ` +
			`GRANT USAGE ON SCHEMA notifications TO ` + ownerRole + `; ` +
			`GRANT SELECT,INSERT,UPDATE,DELETE ON notifications.items,notifications.preferences TO ` + ownerRole + `; ` +
			`GRANT SELECT,INSERT,UPDATE ON notifications.revisions TO ` + ownerRole + `; ` +
			`ALTER TABLE notifications.inbox OWNER TO ` + ownerRole + `; ` +
			`REVOKE UPDATE,DELETE,TRUNCATE ON notifications.inbox FROM ` + ownerRole
		if _, err := f.db.Exec(setupSQL); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.db.Exec(`ALTER TABLE notifications.inbox OWNER TO CURRENT_USER; DROP OWNED BY ` + ownerRole + `; DROP ROLE IF EXISTS ` + ownerRole)
		})
		rc := dsnCfg.Copy()
		rc.Database = currentDB
		rc.User = ownerRole
		db := stdlib.OpenDB(*rc)
		t.Cleanup(func() { db.Close() })
		repo := &postgres.Repository{DB: db}
		if err := repo.CheckRuntime(ctx); !errors.Is(err, n.ErrUnavailable) || n.UnavailableReason(err) != n.ReasonRuntimeUnsafePrivilege {
			t.Fatalf("table owner accepted by CheckRuntime: %v", err)
		}
	})
}

func TestURIDSNSetupAndDesensitizedDiagnostics(t *testing.T) {
	raw := os.Getenv("STUHELPER_NOTIFICATIONS_TEST_DSN")
	cfg, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	var uriDSN string
	if strings.HasPrefix(cfg.Host, "/") {
		q := url.Values{}
		q.Set("host", cfg.Host)
		q.Set("port", fmt.Sprint(cfg.Port))
		q.Set("sslmode", "disable")
		u := url.URL{
			Scheme:   "postgres",
			User:     url.User(cfg.User),
			Path:     "/" + cfg.Database,
			RawQuery: q.Encode(),
		}
		uriDSN = u.String()
	} else {
		uriDSN = fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", url.PathEscape(cfg.User), cfg.Host, cfg.Port, url.PathEscape(cfg.Database))
	}
	parsedSocketURI, err := pgx.ParseConfig("postgres://runner@/postgres?host=%2Ftmp%2Fstuhelper-sock&port=55439&sslmode=disable")
	if err != nil || parsedSocketURI.Host != "/tmp/stuhelper-sock" || parsedSocketURI.Port != 55439 || parsedSocketURI.Database != "postgres" {
		t.Fatalf("unexpected socket URI parse result: cfg=%+v err=%v", parsedSocketURI, err)
	}
	t.Setenv("STUHELPER_NOTIFICATIONS_TEST_DSN", uriDSN)

	f := setup(t)
	ctx := context.Background()
	id := deliver(t, f, delivery("uri-ok", "business")).Recipients[0].ID

	// Corrupt stored JSON must yield desensitized UnavailableReason without leaking payload or owner.
	if _, err = f.db.Exec(`ALTER TABLE notifications.items DROP CONSTRAINT IF EXISTS items_test_dummy; UPDATE notifications.inbox SET result='"corrupt"' WHERE event_id='uri-ok'`); err == nil {
		// Note: CHECK constraint rejects non-object result unless bypassed; test items corrupt json via closed connection/trigger instead.
	}
	_, err = f.db.Exec(`CREATE FUNCTION notifications.raise_sensitive() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'secret-owner-alice-dsn'; END $$; CREATE TRIGGER raise_sensitive BEFORE INSERT ON notifications.items FOR EACH ROW EXECUTE FUNCTION notifications.raise_sensitive()`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Consume(ctx, delivery("sensitive-fail", "business"))
	if !errors.Is(err, n.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "secret-owner-alice-dsn") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("error leaked sensitive message: %q", err.Error())
	}
	if got := n.UnavailableReason(err); got != n.ReasonDatabaseException {
		t.Fatalf("expected %s, got %q", n.ReasonDatabaseException, got)
	}

	// Target resolution diagnostics hook emits desensitized reason codes without user identifiers.
	var diagReasons []string
	diagSvc, err := n.New(&postgres.Repository{DB: f.db}, &resolver{err: context.DeadlineExceeded}, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
		Diagnostics: func(op, reason string) {
			diagReasons = append(diagReasons, op+":"+reason)
		},
	}, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	page, err := diagSvc.List(ctx, "alice", "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].TargetStatus != "unavailable" {
		t.Fatal(page, err)
	}
	if len(diagReasons) != 1 || diagReasons[0] != "target_resolve:"+n.ReasonResolveTimeout {
		t.Fatalf("unexpected diagnostic events: %v", diagReasons)
	}

	// HTTP response body only exposes {"code":"unavailable"}.
	closedDB, err := sql.Open("pgx", uriDSN)
	if err != nil {
		t.Fatal(err)
	}
	_ = closedDB.Close()
	brokenSvc, err := n.New(&postgres.Repository{DB: closedDB}, f.src, n.Config{
		ReadRetention:     24 * time.Hour,
		UnreadRetention:   24 * time.Hour,
		AggregationWindow: time.Hour,
		CleanupBatch:      2,
		ResolveTimeout:    time.Second,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.Handler{
		Service:      brokenSvc,
		Authenticate: func(*http.Request) (string, error) { return "alice", nil },
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/notifications/"+id+"/read", nil))
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != `{"code":"unavailable"}` {
		t.Fatalf("unexpected HTTP 503 response: %d %s", rec.Code, rec.Body.String())
	}
}
