package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/enerplanet/tentacron/internal/config"
	"github.com/enerplanet/tentacron/internal/store"
)

func scheduleEnv(t *testing.T) *testEnv {
	t.Helper()
	return newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys,
			config.APIKey{Name: "other", Key: "other-key", Role: config.RoleClient},
			config.APIKey{Name: "ops", Key: "ops-key", Role: config.RoleAdmin})
	}, nil)
}

func TestScheduleCRUDAndScoping(t *testing.T) {
	e := scheduleEnv(t)
	other := map[string]string{"X-API-Key": "other-key"}
	admin := map[string]string{"X-API-Key": "ops-key"}
	rec := e.do(t, "POST", "/v1/schedules", `{"target":"meme","payload":{"a":1},"cron":"30 6 * * *","timezone":"Europe/Berlin","priority":2,"options":{"cache":"bypass"}}`, authHdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	sc := decodeBody[scheduleResponse](t, rec)
	if sc.ID == "" || sc.Cron != "30 6 * * *" || sc.Timezone != "Europe/Berlin" || sc.Priority != 2 || sc.Options == nil || sc.Options.Cache != "bypass" ||
		sc.Links["runs"] != "/v1/schedules/"+sc.ID+"/runs" || sc.LastRunAt != nil {
		t.Errorf("created = %+v", sc)
	}
	next, err := time.Parse(time.RFC3339, sc.NextRunAt)
	if err != nil || !next.After(time.Now()) || time.Until(next) > 24*time.Hour {
		t.Errorf("next_run_at = %s", sc.NextRunAt)
	}
	stored, _ := e.store.GetSchedule(context.Background(), sc.ID)
	if stored == nil || stored.Client != "valid" && stored.Client == "" {
		t.Fatalf("stored = %+v", stored)
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID, "", authHdr); rec.Code != http.StatusOK {
		t.Errorf("get: %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID, "", other); rec.Code != http.StatusNotFound {
		t.Errorf("another client must see 404: %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID, "", admin); rec.Code != http.StatusOK {
		t.Errorf("admin get: %d", rec.Code)
	}
	if items := decodeBody[map[string][]scheduleResponse](t, e.do(t, "GET", "/v1/schedules", "", other))["items"]; len(items) != 0 {
		t.Errorf("another client's list = %v", items)
	}
	if items := decodeBody[map[string][]scheduleResponse](t, e.do(t, "GET", "/v1/schedules", "", admin))["items"]; len(items) != 1 {
		t.Errorf("admin list = %v", items)
	}
	if items := decodeBody[map[string][]scheduleResponse](t, e.do(t, "GET", "/v1/schedules?client="+stored.Client, "", admin))["items"]; len(items) != 1 {
		t.Errorf("admin list by client = %v", items)
	}
	// Runs: a job under the schedule's key prefix shows up, newest first.
	due := time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)
	for _, d := range []time.Time{due, due.Add(24 * time.Hour)} {
		id, _ := store.NewID()
		if _, _, err := e.store.CreateJob(context.Background(), &store.Job{ID: id, Client: stored.Client, IdempotencyKey: store.RunKey(sc.ID, d), Target: "meme", MaxAttempts: 1, Payload: []byte(`{"a":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	page := decodeBody[listResponse](t, e.do(t, "GET", "/v1/schedules/"+sc.ID+"/runs?limit=1", "", authHdr))
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Errorf("runs page = %+v", page)
	}
	page = decodeBody[listResponse](t, e.do(t, "GET", "/v1/schedules/"+sc.ID+"/runs?cursor="+page.NextCursor, "", authHdr))
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Errorf("runs page 2 = %+v", page)
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID+"/runs", "", other); rec.Code != http.StatusNotFound {
		t.Errorf("another client's runs: %d", rec.Code)
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID+"/runs?state=bogus", "", authHdr); rec.Code != http.StatusBadRequest {
		t.Errorf("bad state filter: %d", rec.Code)
	}
	if rec := e.do(t, "DELETE", "/v1/schedules/"+sc.ID, "", other); rec.Code != http.StatusNotFound {
		t.Errorf("another client's delete: %d", rec.Code)
	}
	if rec := e.do(t, "DELETE", "/v1/schedules/"+sc.ID, "", authHdr); rec.Code != http.StatusNoContent {
		t.Errorf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do(t, "GET", "/v1/schedules/"+sc.ID, "", authHdr); rec.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", rec.Code)
	}
	if runs, _ := e.store.ListJobs(context.Background(), store.ListFilter{IdempotencyPrefix: store.RunKeyPrefix(sc.ID), Limit: 10}); len(runs) != 2 {
		t.Errorf("runs must outlive the schedule: %d", len(runs))
	}
}

func TestScheduleValidation(t *testing.T) {
	zero := 0
	e := newEnvWith(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKey{Name: "capped", Key: "capped-key", Role: config.RoleClient, MaxPriority: &zero})
	}, nil)
	cases := []struct {
		body   string
		hdr    map[string]string
		status int
		code   string
		want   string
	}{
		{`{"target":"meme","payload":{}}`, authHdr, 400, CodeInvalidParameter, "cron expression is required"},
		{`{"target":"meme","payload":{},"cron":"61 * * * *"}`, authHdr, 400, CodeInvalidParameter, "above maximum"},
		{`{"target":"meme","payload":{},"cron":"@daily","timezone":"Mars/Olympus"}`, authHdr, 400, CodeInvalidParameter, "unknown"},
		{`{"target":"meme","payload":{},"cron":"TZ=UTC * * * * *"}`, authHdr, 400, CodeInvalidParameter, "timezone field"},
		{`{"target":"hydra","payload":{},"cron":"@daily"}`, authHdr, 422, CodeUnknownTarget, "not configured"},
		{`{"target":"meme","payload":[1],"cron":"@daily"}`, authHdr, 400, CodeInvalidJSON, "object"},
		{`{"payload":{},"cron":"@daily"}`, authHdr, 400, CodeMissingField, "target"},
		{`{"target":"meme","payload":{},"cron":"@daily","priority":1}`, map[string]string{"X-API-Key": "capped-key"}, 400, CodeInvalidParameter, "maximum"},
		{`{"target":"meme","payload":{},"cron":"@daily"}`, nil, 401, CodeUnauthorized, ""},
	}
	for _, c := range cases {
		rec := e.do(t, "POST", "/v1/schedules", c.body, c.hdr)
		if rec.Code != c.status || errCode(t, rec) != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s, want %d %s %q", c.body, rec.Code, rec.Body.String(), c.status, c.code, c.want)
		}
	}
	if rec := e.do(t, "POST", "/v1/schedules", `{"api_key":"valid-key","target":"meme","payload":{},"cron":"@every 1h"}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("body key fallback: %d %s", rec.Code, rec.Body.String())
	}
}
