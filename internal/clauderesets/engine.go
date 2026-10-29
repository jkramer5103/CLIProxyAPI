package clauderesets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/gofrs/flock"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const StatusPath = "/api/oauth/usage?cedar_ember=1&skip_spend=1"
const ProfilePath = "/api/oauth/profile"

var ErrPaused = errors.New("reset automation paused: unresolved claim or journal unavailable")

type Entry struct {
	RequestID string    `json:"request_id"`
	GrantID   string    `json:"grant_id"`
	Outcome   string    `json:"outcome"` // Empty means potentially dispatched: never automatically retry.
	Next      time.Time `json:"next"`
}

// Engine serializes manual and automatic claims. Journal failures always fail closed.
// Journal keys are hashes of stable credential IDs, never tokens or display names.
type Engine struct {
	mu  sync.Mutex
	Dir string
	Now func() time.Time
}

func New(dir string) *Engine { return &Engine{Dir: dir, Now: time.Now} }

// Lock coordinates all manual/automatic users and other processes sharing this journal.
// A competing process is skipped rather than waited on. No lock file is removed.
func (e *Engine) Lock() (func(), error) {
	e.mu.Lock()
	fail := func() (func(), error) { e.mu.Unlock(); return nil, ErrPaused }
	if e.Dir == "" || os.MkdirAll(e.Dir, 0700) != nil {
		return fail()
	}
	info, err := os.Lstat(e.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fail()
	}
	lock := flock.New(filepath.Join(e.Dir, ".lock"), flock.SetPermissions(0600))
	ok, err := lock.TryLock()
	if err != nil || !ok {
		_ = lock.Close()
		return fail()
	}
	return func() { _ = lock.Unlock(); e.mu.Unlock() }, nil
}
func (e *Engine) path(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(e.Dir, hex.EncodeToString(sum[:])+".json")
}
func (e *Engine) Ready(id string) error {
	b, err := os.ReadFile(e.path(id))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return ErrPaused
	}
	var v Entry
	if json.Unmarshal(b, &v) != nil || !RequestID.MatchString(v.RequestID) || !GrantID.MatchString(v.GrantID) || v.Outcome == "" || Settled(200, []byte(`{"result":"`+v.Outcome+`"}`)) == "" && v.Outcome != "rate_limited" && v.Outcome != "auth_error" {
		return ErrPaused
	}
	if e.Now().Before(v.Next) {
		return ErrPaused
	}
	return nil
}
func (e *Engine) save(id string, v Entry) error {
	if e.Dir == "" {
		return ErrPaused
	}
	if err := os.MkdirAll(e.Dir, 0700); err != nil {
		return ErrPaused
	}
	info, err := os.Lstat(e.Dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return ErrPaused
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ErrPaused
	}
	f, err := os.CreateTemp(e.Dir, ".claim-*")
	if err != nil {
		return ErrPaused
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrPaused
	}
	if os.Rename(name, e.path(id)) != nil {
		return ErrPaused
	}
	d, err := os.Open(e.Dir)
	if err != nil {
		return ErrPaused
	}
	err = d.Sync()
	closeErr = d.Close()
	if err != nil || closeErr != nil {
		return ErrPaused
	}
	return nil
}
func (e *Engine) Begin(id, grant, request string) (Entry, error) {
	if !GrantID.MatchString(grant) || !RequestID.MatchString(request) {
		return Entry{}, ErrMalformed
	}
	if err := e.Ready(id); err != nil {
		return Entry{}, err
	}
	v := Entry{RequestID: request, GrantID: grant}
	return v, e.save(id, v)
}
func (e *Engine) Finish(id string, v Entry, code int, body []byte) error {
	v.Outcome = Settled(code, body)
	// At most one claim per account per minute, including safe refusals and success.
	v.Next = e.Now().Add(time.Minute)
	return e.save(id, v)
}

type Call func(context.Context, string, string, []byte) (int, []byte, error)

// RunAccount is called once per minute. All preflight reads are fresh; any changed
// configuration, credential session, or cancellation invalidates the attempt.
func (e *Engine) RunAccount(ctx context.Context, id string, valid func() bool, call Call) {
	unlock, errLock := e.Lock()
	if errLock != nil {
		return
	}
	defer unlock()
	allowed := func() bool { return ctx.Err() == nil && valid() }
	if !allowed() || e.Ready(id) != nil {
		return
	}
	read := func(path string) ([]byte, error) {
		if !allowed() {
			return nil, ErrPaused
		}
		code, b, err := call(ctx, "GET", path, nil)
		if err != nil || code < 200 || code >= 300 {
			return nil, ErrMalformed
		}
		return b, nil
	}
	b, err := read(StatusPath)
	if err != nil {
		return
	}
	s, err := Parse(b)
	if err != nil || s.Select(e.Now()) == nil {
		return
	}
	b, err = read(ProfilePath)
	if err != nil {
		return
	}
	var p struct {
		Organization struct {
			UUID string `json:"uuid"`
		} `json:"organization"`
	}
	if json.Unmarshal(b, &p) != nil || !OrganizationID.MatchString(p.Organization.UUID) {
		return
	}
	b, err = read(StatusPath)
	if err != nil {
		return
	}
	s, err = Parse(b)
	if err != nil {
		return
	}
	g := s.Select(e.Now())
	if g == nil || !allowed() {
		return
	}
	var random [16]byte
	if _, err = io.ReadFull(rand.Reader, random[:]); err != nil {
		return
	}
	request := hex.EncodeToString(random[:])
	payload, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": g.ID, "request_id": request})
	v, err := e.Begin(id, g.ID, request)
	if err != nil {
		return
	}
	// If invalidated after the durable intent, leave the conservative pending marker.
	if !allowed() {
		return
	}
	code, body, err := call(ctx, "POST", "/api/organizations/"+p.Organization.UUID+"/reset_rate_limits", payload)
	if err != nil {
		return
	} // Transport/read/cancellation uncertainty remains durable pending.
	_ = e.Finish(id, v, code, body)
}
