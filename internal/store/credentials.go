package store

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"skim/internal/domain"
)

// Credentials live only in the private store document, never in Settings or a model prompt.
type providerKey struct {
	Key      string `json:"key"`
	BaseURL  string `json:"baseUrl"`
	Kind     string `json:"kind"`
	Revision string `json:"revision"`
}

func providerKind(p domain.ModelProvider) string {
	if p.Kind == "" {
		return "openai"
	}
	return p.Kind
}

func sameDestination(key providerKey, p domain.ModelProvider) bool {
	return key.BaseURL == strings.TrimRight(p.BaseURL, "/") && key.Kind == providerKind(p)
}

func (d Data) runtimeSettings() domain.Settings {
	s := cloneSettings(d.Settings)
	s.CredentialStoragePath = d.storagePath
	if s.Providers == nil {
		s.Providers = []domain.ModelProvider{}
	}
	if s.ACP.Args == nil {
		s.ACP.Args = []string{}
	}
	for i := range s.Providers {
		p := &s.Providers[i]
		p.CredentialRevision = ""
		p.CredentialStatus = "missing"
		if key, ok := d.ProviderKeys[p.ID]; ok && sameDestination(key, *p) {
			p.CredentialRevision = key.Revision
			p.CredentialStatus = "saved"
		} else if strings.TrimSpace(os.Getenv(p.APIKeyEnv)) != "" {
			p.CredentialStatus = "environment"
		}
	}
	return s
}

// ApplySettings changes this detached candidate only. Commit publishes settings and keys atomically.
func (d *Data) ApplySettings(settings domain.Settings, updates []domain.ProviderKeyUpdate) error {
	if settings.ACP.ManagedRoot == "" {
		settings.ACP.ManagedRoot = filepath.Join(filepath.Dir(d.storagePath), "workspaces")
	}
	changes := make(map[string]domain.ProviderKeyUpdate, len(updates))
	for _, update := range updates {
		if _, exists := changes[update.ProviderID]; exists {
			return errors.New("only one API key update per provider is allowed")
		}
		update.APIKey = strings.TrimSpace(update.APIKey)
		if update.Remove && update.APIKey != "" {
			return errors.New("choose either replacing or removing the saved API key")
		}
		if len(update.APIKey) > 8192 || strings.ContainsAny(update.APIKey, "\r\n\x00") {
			return errors.New("API key must be a single line of at most 8192 bytes")
		}
		changes[update.ProviderID] = update
	}
	keys := maps.Clone(d.ProviderKeys)
	if keys == nil {
		keys = make(map[string]providerKey)
	}
	retained := make(map[string]bool, len(settings.Providers))
	for _, p := range settings.Providers {
		retained[p.ID] = true
		update := changes[p.ID]
		delete(changes, p.ID)
		switch {
		case update.Remove:
			delete(keys, p.ID)
		case update.APIKey != "":
			old := keys[p.ID]
			if old.Key != update.APIKey || !sameDestination(old, p) {
				keys[p.ID] = providerKey{Key: update.APIKey, BaseURL: strings.TrimRight(p.BaseURL, "/"), Kind: providerKind(p), Revision: domain.NewID()}
			}
		default:
			if key, ok := keys[p.ID]; ok && !sameDestination(key, p) {
				return errors.New("changing a provider URL or protocol requires replacing or explicitly removing its saved API key")
			}
		}
	}
	if len(changes) != 0 {
		return errors.New("API key update refers to an unknown provider")
	}
	for id := range keys {
		if !retained[id] {
			delete(keys, id)
		}
	}
	d.Settings = cloneSettings(settings)
	for i := range d.Settings.Providers {
		d.Settings.Providers[i].Name = strings.TrimSpace(d.Settings.Providers[i].Name)
	}
	d.ProviderKeys = keys
	d.Settings = d.runtimeSettings()
	return nil
}

// ResolveCredential refuses stale snapshots rather than forwarding a replacement key.
func (s *Store) ResolveCredential(p domain.ModelProvider) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p.CredentialRevision == "" {
		return "", nil
	}
	key, ok := s.data.ProviderKeys[p.ID]
	if !ok || key.Revision != p.CredentialRevision || !sameDestination(key, p) {
		return "", errors.New("provider credentials changed; retry with current Settings")
	}
	return key.Key, nil
}

func (s *Store) Redact(message string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range s.data.ProviderKeys {
		if key.Key != "" {
			message = strings.ReplaceAll(message, key.Key, "[redacted]")
		}
	}
	names := []string{s.data.Settings.GitHubTokenEnv}
	for _, p := range s.data.Settings.Providers {
		names = append(names, p.APIKeyEnv)
	}
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

// UpdateSettings builds the analysis generation under the same lock as the
// credential transaction. Old jobs can commit until, but never after, cutover.
// committed distinguishes a rename from a subsequent directory-sync failure.
func (s *Store) UpdateSettings(settings domain.Settings, updates []domain.ProviderKeyUpdate, prepare func(*Record, domain.Settings)) (committed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := cloneData(s.data)
	if err := d.ApplySettings(settings, updates); err != nil {
		return false, err
	}
	if r, ok := d.Reviews[d.Active]; ok && r.State.PullRequest != nil {
		prepare(&r, d.Settings)
		d.Reviews[d.Active] = r
	}
	before := s.commitRevision
	err = s.commitLocked(d)
	return s.commitRevision != before, err
}
