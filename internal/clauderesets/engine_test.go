package clauderesets

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var instant = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(`{"eligible":true,"at_limit":true,"grants":[{"id":"grant_1","resets_total":3,"resets_left":2,"ends_at":"2026-10-29T12:05:00Z","usable_now":true}]}`), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func encoded(m map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"cedar_ember": m})
	return b
}
func grant(m map[string]any) map[string]any { return m["grants"].([]any)[0].(map[string]any) }
func TestSelectionRefusingDefaultsAndBounds(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   bool
	}{
		{"five minute boundary", func(map[string]any) {}, true},
		{"inside", func(m map[string]any) { grant(m)["ends_at"] = "2026-10-29T12:00:01Z" }, true},
		{"expired", func(m map[string]any) { grant(m)["ends_at"] = "2026-10-29T12:00:00Z" }, false},
		{"too early", func(m map[string]any) { grant(m)["ends_at"] = "2026-10-29T12:05:00.001Z" }, false},
		{"unknown expiry", func(m map[string]any) { delete(grant(m), "ends_at") }, false},
		{"future start", func(m map[string]any) { grant(m)["starts_at"] = "2026-10-29T12:00:01Z" }, false},
		{"ineligible", func(m map[string]any) { m["eligible"] = false }, false},
		{"cooldown", func(m map[string]any) { m["cooldown_until"] = "2026-10-29T12:00:01Z" }, false},
		{"ended cooldown", func(m map[string]any) { m["cooldown_until"] = "2026-10-29T12:00:00Z" }, true},
		{"paused", func(m map[string]any) { grant(m)["paused"] = true }, false},
		{"unusable", func(m map[string]any) { grant(m)["usable_now"] = false }, false},
		{"missing usability", func(m map[string]any) { delete(grant(m), "usable_now") }, false},
		{"exhausted", func(m map[string]any) { grant(m)["resets_left"] = 0 }, false},
		{"missing at limit", func(m map[string]any) { delete(m, "at_limit") }, false},
		{"not limited", func(m map[string]any) { m["at_limit"] = false }, false},
		{"limit not required", func(m map[string]any) { m["at_limit"] = false; grant(m)["use_requires_limit"] = false }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fixture(t)
			tc.mutate(m)
			s, err := Parse(encoded(m))
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Select(instant) != nil; got != tc.want {
				t.Fatalf("selected=%v want %v", got, tc.want)
			}
		})
	}
}
func TestMalformedRejectsWholeBlock(t *testing.T) {
	for _, field := range []string{"eligible", "at_limit", "cooldown_until", "weekly_resets_at", "ineligible_reason"} {
		t.Run(field, func(t *testing.T) {
			m := fixture(t)
			m[field] = []any{}
			if _, err := Parse(encoded(m)); err == nil {
				t.Fatal("accepted malformed block")
			}
		})
	}
	for _, field := range []string{"id", "resets_total", "resets_left", "starts_at", "ends_at", "paused", "usable_now", "use_requires_limit", "clears"} {
		t.Run(field, func(t *testing.T) {
			m := fixture(t)
			grant(m)[field] = map[string]any{}
			if _, err := Parse(encoded(m)); err == nil {
				t.Fatal("accepted malformed grant")
			}
		})
	}
	for _, raw := range []string{`{}`, `null`, `{"cedar_ember":null}`, `{"cedar_ember":[]}`, `{"cedar_ember":{"grants":[]}}`} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Fatal("accepted unsupported status")
		}
	}
	for _, mutation := range []func(map[string]any){func(m map[string]any) { m["grants"] = append(m["grants"].([]any), grant(m)) }, func(m map[string]any) { grant(m)["resets_left"] = 4 }, func(m map[string]any) { grant(m)["resets_total"] = -1 }, func(m map[string]any) { grant(m)["resets_left"] = 0.5 }, func(m map[string]any) { delete(grant(m), "resets_left") }, func(m map[string]any) { grant(m)["id"] = "../x" }} {
		m := fixture(t)
		mutation(m)
		if _, err := Parse(encoded(m)); err == nil {
			t.Fatal("accepted invalid counts or duplicate/id")
		}
	}
}
func TestEarliestEligibleOrdering(t *testing.T) {
	m := fixture(t)
	b, _ := json.Marshal(grant(m))
	var earlier map[string]any
	_ = json.Unmarshal(b, &earlier)
	earlier["id"] = "earlier"
	earlier["ends_at"] = "2026-10-29T12:02:00Z"
	m["grants"] = append(m["grants"].([]any), earlier)
	s, err := Parse(encoded(m))
	if err != nil || s.Select(instant).ID != "earlier" {
		t.Fatal("did not choose earliest")
	}
	s.Grants[1].Paused = new(bool)
	*s.Grants[1].Paused = true
	if s.Select(instant).ID != "grant_1" {
		t.Fatal("did not skip paused earliest")
	}
}
func engine(t *testing.T) *Engine {
	e := New(t.TempDir() + "/journal")
	e.Now = func() time.Time { return instant }
	return e
}
func TestUncertainOutcomesSurviveRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
		err  error
	}{
		{"timeout", 0, "", context.DeadlineExceeded}, {"canceled", 0, "", context.Canceled}, {"server failure", 500, `{}`, nil}, {"unknown result", 200, `{"result":"new_result"}`, nil}, {"bad body", 200, `broken`, nil}, {"redirect", 307, ``, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := engine(t)
			claims := 0
			request := ""
			call := func(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
				if method == "POST" {
					claims++
					var p map[string]string
					_ = json.Unmarshal(body, &p)
					request = p["request_id"]
					b, err := os.ReadFile(e.path("account"))
					if err != nil || !strings.Contains(string(b), request) {
						t.Fatal("claim dispatched without durable original request id")
					}
					return tc.code, []byte(tc.body), tc.err
				}
				if path == ProfilePath {
					return 200, []byte(`{"organization":{"uuid":"12345678-1234-1234-1234-123456789abc"}}`), nil
				}
				return 200, encoded(fixture(t)), nil
			}
			e.RunAccount(context.Background(), "account", func() bool { return true }, call)
			restarted := New(e.Dir)
			restarted.Now = func() time.Time { return instant.Add(24 * time.Hour) }
			restarted.RunAccount(context.Background(), "account", func() bool { return true }, call)
			if claims != 1 || request == "" {
				t.Fatalf("claims=%d request missing=%v", claims, request == "")
			}
			info, _ := os.Stat(e.path("account"))
			if info.Mode().Perm() != 0600 {
				t.Fatal("journal permissions")
			}
		})
	}
}
func TestSafeAnswersBoundedCadenceAndFreshLimits(t *testing.T) {
	for _, result := range []string{"reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable", "rate_limited", "auth_error"} {
		t.Run(result, func(t *testing.T) {
			e := engine(t)
			now := instant
			e.Now = func() time.Time { return now }
			claims := 0
			reads := 0
			call := func(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
				if method == "POST" {
					claims++
					switch result {
					case "rate_limited":
						return 429, nil, nil
					case "auth_error":
						return 401, nil, nil
					}
					return 200, []byte(`{"result":"` + result + `"}`), nil
				}
				if path == ProfilePath {
					return 200, []byte(`{"organization":{"uuid":"12345678-1234-1234-1234-123456789abc"}}`), nil
				}
				reads++
				m := fixture(t)
				if claims > 0 && result == "reset" {
					m["at_limit"] = false
				}
				return 200, encoded(m), nil
			}
			e.RunAccount(context.Background(), "a", func() bool { return true }, call)
			e.RunAccount(context.Background(), "a", func() bool { return true }, call)
			if claims != 1 || reads != 2 {
				t.Fatal("unbounded retry")
			}
			now = now.Add(time.Minute)
			e.RunAccount(context.Background(), "a", func() bool { return true }, call)
			want := 2
			if result == "reset" {
				want = 1
			}
			if claims != want {
				t.Fatal("fresh limits not respected or safe retry blocked")
			}
		})
	}
}
func TestCancellationAndPreflightChanges(t *testing.T) {
	for _, stage := range []int{0, 1, 2, 3} {
		for _, cancelContext := range []bool{false, true} {
			e := engine(t)
			ctx, cancel := context.WithCancel(context.Background())
			valid := true
			calls := 0
			claims := 0
			invalidate := func() {
				if cancelContext {
					cancel()
				} else {
					valid = false
				}
			}
			if stage == 0 {
				invalidate()
			}
			call := func(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
				calls++
				if method == "POST" {
					claims++
				}
				if calls == stage {
					invalidate()
				}
				if path == ProfilePath {
					return 200, []byte(`{"organization":{"uuid":"12345678-1234-1234-1234-123456789abc"}}`), nil
				}
				return 200, encoded(fixture(t)), nil
			}
			e.RunAccount(ctx, "a", func() bool { return valid }, call)
			cancel()
			if claims != 0 {
				t.Fatalf("claim after invalidation at stage %d", stage)
			}
		}
	}
}
func TestJournalFailClosedAndManualSerialization(t *testing.T) {
	e := engine(t)
	unlock, errLock := e.Lock()
	if errLock != nil {
		t.Fatal(errLock)
	}
	v, err := e.Begin("a", "grant_1", "manual_original")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	e.RunAccount(context.Background(), "a", func() bool { return true }, func(context.Context, string, string, []byte) (int, []byte, error) {
		t.Fatal("automatic call during pending manual claim")
		return 0, nil, nil
	})
	if err = e.Finish("a", v, 200, []byte(`{"result":"reset"}`)); err != nil {
		t.Fatal(err)
	}
	if e.Ready("a") == nil {
		t.Fatal("missing bounded cadence")
	}
	if err = os.WriteFile(e.path("a"), []byte("corrupted"), 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(e.Ready("a"), ErrPaused) {
		t.Fatal("corruption not paused")
	}
	if err = os.Chmod(e.Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Begin("other", "grant_1", "id"); err == nil {
		t.Fatal("unsafe permissions accepted")
	}
}

func TestCrossProcessJournalLock(t *testing.T) {
	e := engine(t)
	unlock, err := e.Lock()
	if err != nil {
		t.Fatal(err)
	}
	other := New(e.Dir)
	if release, errOther := other.Lock(); errOther == nil {
		release()
		t.Fatal("competing engine acquired journal lock")
	}
	unlock()
	release, err := other.Lock()
	if err != nil {
		t.Fatal("lock not released", err)
	}
	release()
}
