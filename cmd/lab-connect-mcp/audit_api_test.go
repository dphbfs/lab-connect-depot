package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newTestAuditLogWithRows(t *testing.T, n int) *auditLog {
	t.Helper()
	audit, err := openAuditLog(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("openAuditLog() error: %v", err)
	}
	t.Cleanup(func() { audit.db.Close() })
	for i := 0; i < n; i++ {
		audit.Record(context.Background(), auditCall{Tool: "machines", Status: 0})
	}
	return audit
}

func TestAuditListHandler_DefaultsAndClamping(t *testing.T) {
	audit := newTestAuditLogWithRows(t, 3)
	h := auditListHandler(audit)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/audit-log", nil))

	var got auditListResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Total != 3 {
		t.Fatalf("total = %d, want 3", got.Total)
	}
	if len(got.Rows) != 3 {
		t.Fatalf("rows = %d, want 3 (default limit covers all of them)", len(got.Rows))
	}
}

func TestAuditListHandler_PaginatesNewestFirst(t *testing.T) {
	audit := newTestAuditLogWithRows(t, 5)
	h := auditListHandler(audit)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/audit-log?limit=2&offset=0", nil))
	var page1 auditListResponse
	json.NewDecoder(rec.Body).Decode(&page1)
	if len(page1.Rows) != 2 {
		t.Fatalf("page1 rows = %d, want 2", len(page1.Rows))
	}
	if page1.Total != 5 {
		t.Fatalf("total = %d, want 5", page1.Total)
	}
	if page1.Rows[0].ID <= page1.Rows[1].ID {
		t.Fatalf("rows must be newest-first, got ids %d then %d", page1.Rows[0].ID, page1.Rows[1].ID)
	}

	rec2 := httptest.NewRecorder()
	h(rec2, httptest.NewRequest(http.MethodGet, "/api/audit-log?limit=2&offset=2", nil))
	var page2 auditListResponse
	json.NewDecoder(rec2.Body).Decode(&page2)
	if page1.Rows[1].ID <= page2.Rows[0].ID {
		t.Fatalf("page2's first row (id %d) must come after page1's last row (id %d)", page2.Rows[0].ID, page1.Rows[1].ID)
	}
}

func TestAuditListHandler_InvalidLimitFallsBackToDefault(t *testing.T) {
	audit := newTestAuditLogWithRows(t, 1)
	h := auditListHandler(audit)

	for _, limit := range []string{"0", "-5", "abc", "999999"} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/api/audit-log?limit="+limit, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("limit=%q: status = %d, want 200", limit, rec.Code)
		}
	}
}

func TestAuditListHandler_NullFieldsRoundTripAsJSONNull(t *testing.T) {
	audit := newTestAuditLogWithRows(t, 1) // "machines" row: no target/command
	h := auditListHandler(audit)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/api/audit-log", nil))

	var got auditListResponse
	json.NewDecoder(rec.Body).Decode(&got)
	row := got.Rows[0]
	if row.TargetName != nil || row.Command != nil {
		t.Fatalf("machines row should have nil target/command, got %+v", row)
	}
}
