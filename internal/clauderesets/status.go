// Package clauderesets implements conservative cedar_ember expiry redemption.
package clauderesets

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"time"
)

var ErrMalformed = errors.New("unsupported reset-grant data")
var GrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
var RequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var OrganizationID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Grant struct {
	ID            string            `json:"id"`
	Total         *int64            `json:"resets_total"`
	Left          *int64            `json:"resets_left"`
	Starts        *time.Time        `json:"starts_at"`
	Ends          *time.Time        `json:"ends_at"`
	Paused        *bool             `json:"paused"`
	Usable        *bool             `json:"usable_now"`
	RequiresLimit *bool             `json:"use_requires_limit"`
	Clears        []json.RawMessage `json:"clears"`
}
type Status struct {
	Eligible *bool      `json:"eligible"`
	AtLimit  *bool      `json:"at_limit"`
	Cooldown *time.Time `json:"cooldown_until"`
	Weekly   *time.Time `json:"weekly_resets_at"`
	Reason   *string    `json:"ineligible_reason"`
	Grants   []Grant    `json:"grants"`
}

func Parse(body []byte) (*Status, error) {
	var envelope struct {
		Block json.RawMessage `json:"cedar_ember"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return nil, ErrMalformed
	}
	var s Status
	if json.Unmarshal(envelope.Block, &s) != nil || s.Eligible == nil {
		return nil, ErrMalformed
	}
	seen := map[string]bool{}
	for _, g := range s.Grants {
		if !GrantID.MatchString(g.ID) || seen[g.ID] || g.Total == nil || g.Left == nil || *g.Total < 0 || *g.Left < 0 || *g.Left > *g.Total {
			return nil, ErrMalformed
		}
		seen[g.ID] = true
	}
	return &s, nil
}
func yes(v *bool) bool { return v != nil && *v }

// Select requires a known expiry and an explicit usable flag. The upper bound is inclusive.
func (s *Status) Select(now time.Time) *Grant {
	if s == nil || !yes(s.Eligible) || (s.Cooldown != nil && s.Cooldown.After(now)) {
		return nil
	}
	var candidates []Grant
	for _, g := range s.Grants {
		if yes(g.Paused) || !yes(g.Usable) || *g.Left <= 0 || g.Ends == nil || !g.Ends.After(now) || g.Ends.After(now.Add(5*time.Minute)) || (g.Starts != nil && g.Starts.After(now)) {
			continue
		}
		if (g.RequiresLimit == nil || *g.RequiresLimit) && !yes(s.AtLimit) {
			continue
		}
		candidates = append(candidates, g)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Ends.Equal(*candidates[j].Ends) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].Ends.Before(*candidates[j].Ends)
	})
	if len(candidates) == 0 {
		return nil
	}
	return &candidates[0]
}

// Settled accepts only explicit terminal results or HTTP refusals proving no spend.
func Settled(code int, body []byte) string {
	if code == 429 {
		return "rate_limited"
	}
	if code == 401 || code == 403 {
		return "auth_error"
	}
	if code < 200 || code >= 300 {
		return ""
	}
	var r struct {
		Result string `json:"result"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	switch r.Result {
	case "reset", "already_used", "not_limited", "cooldown", "ineligible", "unavailable":
		return r.Result
	}
	return ""
}
