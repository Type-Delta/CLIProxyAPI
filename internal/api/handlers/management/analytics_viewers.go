package management

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/cpauk/model"
)

const (
	viewerCredentialBytes = 32
	viewerSessionBytes    = 32
	viewerLabelMaxBytes   = 128
	viewerMaxLifetime     = 90 * 24 * time.Hour
	viewerSessionLifetime = 30 * time.Minute
	viewerMaxRecords      = 4096
	viewerMaxSessions     = 16384
	viewerMaxSessionsEach = 32
	viewerStoreMaxBytes   = 16 << 20
)

var (
	ErrViewerNotFound          = errors.New("analytics viewer not found")
	ErrViewerCredentialInvalid = errors.New("analytics viewer credential is invalid")
	ErrViewerSessionInvalid    = errors.New("analytics viewer session is invalid")
	ErrViewerViewForbidden     = errors.New("analytics viewer view is forbidden")
	ErrViewerCapacity          = errors.New("analytics viewer capacity reached")
	ErrViewerInvalidRequest    = errors.New("analytics viewer request is invalid")
)

var allowedViewerViews = map[string]struct{}{
	"capabilities": {},
	"summary":      {},
	"timeseries":   {},
	"events":       {},
}

type ViewerCreateRequest struct {
	KeyID        string    `json:"key_id"`
	AllowedViews []string  `json:"allowed_views"`
	ExpiresAt    time.Time `json:"expires_at"`
	Label        string    `json:"label,omitempty"`
}

type ViewerCreateResponse struct {
	ID           string    `json:"id"`
	Credential   string    `json:"credential"`
	AllowedViews []string  `json:"allowed_views"`
	ExpiresAt    time.Time `json:"expires_at"`
	Label        string    `json:"label,omitempty"`
}

type ViewerMetadata struct {
	ID           string    `json:"id"`
	KeyID        string    `json:"key_id"`
	AllowedViews []string  `json:"allowed_views"`
	ExpiresAt    time.Time `json:"expires_at"`
	Label        string    `json:"label,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ViewerSessionScope carries both expiries: ExpiresAt is the short-lived
// session expiry (also the cookie lifetime), ViewExpiresAt is the shared
// view/link expiry the creator chose.
type ViewerSessionScope struct {
	AuditID       string
	KeyID         string
	AllowedViews  []string
	ExpiresAt     time.Time
	ViewExpiresAt time.Time
	Label         string
}

type viewerRecord struct {
	ID             string    `json:"id"`
	CredentialHash string    `json:"credential_hash"`
	KeyID          string    `json:"key_id"`
	AllowedViews   []string  `json:"allowed_views"`
	ExpiresAt      time.Time `json:"expires_at"`
	Label          string    `json:"label,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type viewerSessionRecord struct {
	AuditID     string    `json:"audit_id"`
	SessionHash string    `json:"session_hash"`
	ViewerID    string    `json:"viewer_id"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type viewerStoreDocument struct {
	SchemaVersion int                   `json:"schema_version"`
	Viewers       []viewerRecord        `json:"viewers"`
	Sessions      []viewerSessionRecord `json:"sessions"`
}

// AnalyticsViewerStore persists only hashes of credentials and sessions.
// The raw credential is returned once and the raw session token lives only in
// its HttpOnly cookie.
type AnalyticsViewerStore struct {
	mu       sync.RWMutex
	path     string
	now      func() time.Time
	viewers  map[string]viewerRecord
	sessions map[string]viewerSessionRecord
}

func NewAnalyticsViewerStore(path string) (*AnalyticsViewerStore, error) {
	store := &AnalyticsViewerStore{
		path:     strings.TrimSpace(path),
		now:      time.Now,
		viewers:  make(map[string]viewerRecord),
		sessions: make(map[string]viewerSessionRecord),
	}
	if store.path == "" {
		return store, nil
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *AnalyticsViewerStore) Create(request ViewerCreateRequest) (ViewerCreateResponse, error) {
	if s == nil {
		return ViewerCreateResponse{}, ErrViewerCredentialInvalid
	}
	request.Label = strings.TrimSpace(request.Label)
	request.ExpiresAt = request.ExpiresAt.UTC()
	if !model.IsFullKeyID(request.KeyID) {
		return ViewerCreateResponse{}, fmt.Errorf("%w: key_id must be a full key ID", ErrViewerInvalidRequest)
	}
	if len(request.Label) > viewerLabelMaxBytes {
		return ViewerCreateResponse{}, fmt.Errorf("%w: label exceeds %d bytes", ErrViewerInvalidRequest, viewerLabelMaxBytes)
	}
	views, err := normalizeViewerViews(request.AllowedViews)
	if err != nil {
		return ViewerCreateResponse{}, fmt.Errorf("%w: %v", ErrViewerInvalidRequest, err)
	}
	now := s.now().UTC()
	if !request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(viewerMaxLifetime)) {
		return ViewerCreateResponse{}, fmt.Errorf("%w: expires_at must be within the next 90 days", ErrViewerInvalidRequest)
	}
	credential, err := randomToken(viewerCredentialBytes)
	if err != nil {
		return ViewerCreateResponse{}, fmt.Errorf("generate viewer credential: %w", err)
	}
	idBytes := make([]byte, 12)
	if _, err = io.ReadFull(rand.Reader, idBytes); err != nil {
		return ViewerCreateResponse{}, fmt.Errorf("generate viewer ID: %w", err)
	}
	record := viewerRecord{
		ID:             "viewer-" + hex.EncodeToString(idBytes),
		CredentialHash: viewerHash("credential", credential),
		KeyID:          request.KeyID,
		AllowedViews:   views,
		ExpiresAt:      request.ExpiresAt,
		Label:          request.Label,
		CreatedAt:      now,
	}
	s.mu.Lock()
	previousViewers, previousSessions := cloneViewerState(s.viewers, s.sessions)
	s.pruneExpiredLocked(now)
	if len(s.viewers) >= viewerMaxRecords {
		s.viewers, s.sessions = previousViewers, previousSessions
		s.mu.Unlock()
		return ViewerCreateResponse{}, ErrViewerCapacity
	}
	s.viewers[record.ID] = record
	if err = s.persistLocked(); err != nil {
		if !viewerPersistenceCommitted(err) {
			s.viewers, s.sessions = previousViewers, previousSessions
		}
		s.mu.Unlock()
		return ViewerCreateResponse{}, err
	}
	s.mu.Unlock()
	return ViewerCreateResponse{
		ID: record.ID, Credential: credential, AllowedViews: slices.Clone(views),
		ExpiresAt: record.ExpiresAt, Label: record.Label,
	}, nil
}

func (s *AnalyticsViewerStore) List() ([]ViewerMetadata, error) {
	if s == nil {
		return nil, ErrViewerNotFound
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	previousViewers, previousSessions := cloneViewerState(s.viewers, s.sessions)
	s.pruneExpiredLocked(now)
	if len(previousViewers) != len(s.viewers) || len(previousSessions) != len(s.sessions) {
		if err := s.persistLocked(); err != nil && !viewerPersistenceCommitted(err) {
			s.viewers, s.sessions = previousViewers, previousSessions
			return nil, err
		}
	}
	result := make([]ViewerMetadata, 0, len(s.viewers))
	for _, record := range s.viewers {
		result = append(result, ViewerMetadata{
			ID: record.ID, KeyID: record.KeyID, AllowedViews: slices.Clone(record.AllowedViews),
			ExpiresAt: record.ExpiresAt, Label: record.Label, CreatedAt: record.CreatedAt,
		})
	}
	slices.SortFunc(result, func(a, b ViewerMetadata) int {
		if order := b.CreatedAt.Compare(a.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
	return result, nil
}

func (s *AnalyticsViewerStore) Revoke(id string) error {
	if s == nil {
		return ErrViewerNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.viewers[id]; !ok {
		return ErrViewerNotFound
	}
	previousViewers, previousSessions := cloneViewerState(s.viewers, s.sessions)
	delete(s.viewers, id)
	for hash, session := range s.sessions {
		if session.ViewerID == id {
			delete(s.sessions, hash)
		}
	}
	if err := s.persistLocked(); err != nil {
		if !viewerPersistenceCommitted(err) {
			s.viewers, s.sessions = previousViewers, previousSessions
		}
		return err
	}
	return nil
}

func (s *AnalyticsViewerStore) Exchange(credential string) (string, ViewerSessionScope, error) {
	if s == nil || credential == "" || len(credential) > 512 {
		return "", ViewerSessionScope{}, ErrViewerCredentialInvalid
	}
	wantHash := viewerHash("credential", credential)
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	previousViewers, previousSessions := cloneViewerState(s.viewers, s.sessions)
	s.pruneExpiredLocked(now)
	var matched viewerRecord
	for _, record := range s.viewers {
		if constantTimeTextEqual(record.CredentialHash, wantHash) {
			matched = record
		}
	}
	if matched.ID == "" || !matched.ExpiresAt.After(now) {
		s.viewers, s.sessions = previousViewers, previousSessions
		return "", ViewerSessionScope{}, ErrViewerCredentialInvalid
	}
	viewerSessions := 0
	for _, session := range s.sessions {
		if session.ViewerID == matched.ID {
			viewerSessions++
		}
	}
	if len(s.sessions) >= viewerMaxSessions || viewerSessions >= viewerMaxSessionsEach {
		s.viewers, s.sessions = previousViewers, previousSessions
		return "", ViewerSessionScope{}, ErrViewerCapacity
	}
	token, err := randomToken(viewerSessionBytes)
	if err != nil {
		return "", ViewerSessionScope{}, fmt.Errorf("generate viewer session: %w", err)
	}
	auditBytes := make([]byte, 8)
	if _, err = io.ReadFull(rand.Reader, auditBytes); err != nil {
		return "", ViewerSessionScope{}, fmt.Errorf("generate viewer audit ID: %w", err)
	}
	expiresAt := now.Add(viewerSessionLifetime)
	if matched.ExpiresAt.Before(expiresAt) {
		expiresAt = matched.ExpiresAt
	}
	record := viewerSessionRecord{
		AuditID: "vs-" + hex.EncodeToString(auditBytes), SessionHash: viewerHash("session", token),
		ViewerID: matched.ID, ExpiresAt: expiresAt,
	}
	s.sessions[record.SessionHash] = record
	if err = s.persistLocked(); err != nil {
		if !viewerPersistenceCommitted(err) {
			s.viewers, s.sessions = previousViewers, previousSessions
		}
		return "", ViewerSessionScope{}, err
	}
	return token, viewerScope(matched, record), nil
}

func (s *AnalyticsViewerStore) Authenticate(sessionToken, view string) (ViewerSessionScope, error) {
	if s == nil || sessionToken == "" || len(sessionToken) > 512 {
		return ViewerSessionScope{}, ErrViewerSessionInvalid
	}
	hash := viewerHash("session", sessionToken)
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(now)
	session, ok := s.sessions[hash]
	if !ok || !session.ExpiresAt.After(now) {
		return ViewerSessionScope{}, ErrViewerSessionInvalid
	}
	viewer, ok := s.viewers[session.ViewerID]
	if !ok || !viewer.ExpiresAt.After(now) {
		return ViewerSessionScope{}, ErrViewerSessionInvalid
	}
	if view != "" && !slices.Contains(viewer.AllowedViews, view) {
		return ViewerSessionScope{}, ErrViewerViewForbidden
	}
	return viewerScope(viewer, session), nil
}

// InvalidateSessions revokes every active cookie without deleting viewer
// credentials. Configuration reloads call this so old sessions cannot retain
// stale scope.
func (s *AnalyticsViewerStore) InvalidateSessions() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previousViewers, previousSessions := cloneViewerState(s.viewers, s.sessions)
	s.sessions = make(map[string]viewerSessionRecord)
	if err := s.persistLocked(); err != nil {
		if !viewerPersistenceCommitted(err) {
			s.viewers, s.sessions = previousViewers, previousSessions
		}
		return err
	}
	return nil
}

func viewerScope(viewer viewerRecord, session viewerSessionRecord) ViewerSessionScope {
	return ViewerSessionScope{
		AuditID: session.AuditID, KeyID: viewer.KeyID, AllowedViews: slices.Clone(viewer.AllowedViews),
		ExpiresAt: session.ExpiresAt, ViewExpiresAt: viewer.ExpiresAt, Label: viewer.Label,
	}
}

func (s *AnalyticsViewerStore) load() error {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open analytics viewer store: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect analytics viewer store: %w", err)
	}
	if info.Size() < 0 || info.Size() > viewerStoreMaxBytes {
		_ = file.Close()
		return fmt.Errorf("analytics viewer store exceeds its bounds")
	}
	data, err := io.ReadAll(io.LimitReader(file, viewerStoreMaxBytes+1))
	errClose := file.Close()
	if err != nil {
		return fmt.Errorf("read analytics viewer store: %w", err)
	}
	if errClose != nil {
		return fmt.Errorf("close analytics viewer store: %w", errClose)
	}
	if len(data) > viewerStoreMaxBytes {
		return fmt.Errorf("analytics viewer store exceeds its bounds")
	}
	var document viewerStoreDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&document); err != nil || document.SchemaVersion != 1 {
		return fmt.Errorf("decode analytics viewer store")
	}
	if len(document.Viewers) > viewerMaxRecords || len(document.Sessions) > viewerMaxSessions {
		return fmt.Errorf("analytics viewer store exceeds its bounds")
	}
	for _, record := range document.Viewers {
		if record.ID == "" || !model.IsFullKeyID(record.KeyID) || !validStoredHash(record.CredentialHash) {
			return fmt.Errorf("invalid analytics viewer store record")
		}
		if _, errViews := normalizeViewerViews(record.AllowedViews); errViews != nil {
			return fmt.Errorf("invalid analytics viewer store views")
		}
		s.viewers[record.ID] = record
	}
	for _, record := range document.Sessions {
		if record.AuditID == "" || !validStoredHash(record.SessionHash) || record.ViewerID == "" {
			return fmt.Errorf("invalid analytics viewer session record")
		}
		s.sessions[record.SessionHash] = record
	}
	s.pruneExpiredLocked(s.now().UTC())
	return nil
}

func cloneViewerState(viewers map[string]viewerRecord, sessions map[string]viewerSessionRecord) (map[string]viewerRecord, map[string]viewerSessionRecord) {
	viewerCopy := make(map[string]viewerRecord, len(viewers))
	for id, record := range viewers {
		record.AllowedViews = slices.Clone(record.AllowedViews)
		viewerCopy[id] = record
	}
	sessionCopy := make(map[string]viewerSessionRecord, len(sessions))
	for hash, record := range sessions {
		sessionCopy[hash] = record
	}
	return viewerCopy, sessionCopy
}

func (s *AnalyticsViewerStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create analytics viewer directory: %w", err)
	}
	document := viewerStoreDocument{SchemaVersion: 1}
	for _, record := range s.viewers {
		document.Viewers = append(document.Viewers, record)
	}
	for _, record := range s.sessions {
		document.Sessions = append(document.Sessions, record)
	}
	slices.SortFunc(document.Viewers, func(a, b viewerRecord) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(document.Sessions, func(a, b viewerSessionRecord) int { return strings.Compare(a.SessionHash, b.SessionHash) })
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("encode analytics viewer store: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(s.path), ".analytics-viewers-*")
	if err != nil {
		return fmt.Errorf("create analytics viewer temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()
	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(data)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if errClose := temporary.Close(); err == nil {
		err = errClose
	}
	if err != nil {
		return fmt.Errorf("write analytics viewer store: %w", err)
	}
	if err = os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace analytics viewer store: %w", err)
	}
	committed = true
	if err = syncViewerParentDirectory(filepath.Dir(s.path)); err != nil {
		return viewerPersistError{err: fmt.Errorf("sync analytics viewer directory: %w", err), committed: true}
	}
	return nil
}

type viewerPersistError struct {
	err       error
	committed bool
}

func (e viewerPersistError) Error() string { return e.err.Error() }
func (e viewerPersistError) Unwrap() error { return e.err }

func viewerPersistenceCommitted(err error) bool {
	var persistError viewerPersistError
	return errors.As(err, &persistError) && persistError.committed
}

func syncViewerParentDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	if errClose := directory.Close(); err == nil {
		err = errClose
	}
	return err
}

func (s *AnalyticsViewerStore) pruneExpiredLocked(now time.Time) {
	for id, viewer := range s.viewers {
		if !viewer.ExpiresAt.After(now) {
			delete(s.viewers, id)
		}
	}
	for hash, session := range s.sessions {
		if !session.ExpiresAt.After(now) {
			delete(s.sessions, hash)
			continue
		}
		if _, ok := s.viewers[session.ViewerID]; !ok {
			delete(s.sessions, hash)
		}
	}
}

func normalizeViewerViews(input []string) ([]string, error) {
	if len(input) == 0 || len(input) > len(allowedViewerViews) {
		return nil, fmt.Errorf("allowed_views must contain 1 to %d values", len(allowedViewerViews))
	}
	views := make([]string, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, value := range input {
		value = strings.TrimSpace(value)
		if _, ok := allowedViewerViews[value]; !ok {
			return nil, fmt.Errorf("unsupported viewer view %q", value)
		}
		if _, duplicate := seen[value]; !duplicate {
			seen[value] = struct{}{}
			views = append(views, value)
		}
	}
	slices.Sort(views)
	return views, nil
}

func randomToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func viewerHash(kind, token string) string {
	digest := sha256.Sum256([]byte("cpa-analytics-viewer-" + kind + "-v1\x00" + token))
	return hex.EncodeToString(digest[:])
}

func validStoredHash(value string) bool {
	return model.IsFullKeyID(value)
}

func constantTimeTextEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (h *Handler) CreateAnalyticsViewer(c *gin.Context) {
	if _, err := h.analyticsServiceForRead(); err != nil {
		writeAnalyticsError(c, err)
		return
	}
	store := h.analyticsViewerStore()
	if store == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	var request ViewerCreateRequest
	if err := decodeAnalyticsJSON(c, &request, 8*1024); err != nil {
		writeAnalyticsInvalid(c, err)
		return
	}
	response, err := store.Create(request)
	if err != nil {
		if errors.Is(err, ErrViewerCapacity) {
			writeAnalyticsError(c, err)
			return
		}
		if errors.Is(err, ErrViewerInvalidRequest) {
			writeAnalyticsInvalid(c, err)
			return
		}
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.JSON(201, response)
}

func (h *Handler) ListAnalyticsViewers(c *gin.Context) {
	store := h.analyticsViewerStore()
	if store == nil {
		writeAnalyticsError(c, cpauk.ErrUnavailable)
		return
	}
	viewers, err := store.List()
	if err != nil {
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.JSON(http.StatusOK, gin.H{"viewers": viewers})
}

func (h *Handler) DeleteAnalyticsViewer(c *gin.Context) {
	store := h.analyticsViewerStore()
	if store == nil {
		writeAnalyticsError(c, ErrViewerNotFound)
		return
	}
	if err := store.Revoke(c.Param("id")); err != nil {
		writeAnalyticsError(c, err)
		return
	}
	setAnalyticsNoStore(c)
	c.Status(204)
}

func (h *Handler) analyticsViewerStore() *AnalyticsViewerStore {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.analyticsViewers
}
