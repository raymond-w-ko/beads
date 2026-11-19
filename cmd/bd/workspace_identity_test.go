package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/configfile"
	"github.com/steveyegge/beads/internal/storage"
)

func TestValidateWorkspaceIdentity_NilStore(t *testing.T) {
	// When store is nil, validateWorkspaceIdentity should be a no-op
	// (no panic, no os.Exit)
	origStore := store
	store = nil
	defer func() { store = nil; store = origStore }()

	validateWorkspaceIdentity(nil, "/nonexistent")
	// If we got here, no os.Exit was called — pass
}

func TestValidateWorkspaceIdentity_NonexistentDir(t *testing.T) {
	// When beadsDir doesn't exist, configfile.Load fails and we skip validation
	origStore := store
	store = nil
	defer func() { store = origStore }()

	validateWorkspaceIdentity(nil, "/nonexistent/path/that/does/not/exist")
}

// identityProbeStore is a store fake for validateWorkspaceIdentity: it serves
// _project_id and counts how often it is asked.
type identityProbeStore struct {
	storage.DoltStorage
	dbProjectID   string
	metadataReads int
}

func (s *identityProbeStore) GetMetadata(_ context.Context, key string) (string, error) {
	s.metadataReads++
	if key != "_project_id" {
		return "", nil
	}
	return s.dbProjectID, nil
}

// verifyingProbeStore also implements the optional IdentityVerifiedAtOpen,
// reporting verified only for verifiedDir.
type verifyingProbeStore struct {
	identityProbeStore
	verifiedDir string
}

func (s *verifyingProbeStore) IdentityVerifiedAtOpen(beadsDir string) bool {
	return s.verifiedDir != "" && beadsDir == s.verifiedDir
}

// unwrappingStore is a decorator over a store, as the hook decorator is.
type unwrappingStore struct {
	storage.DoltStorage
}

func (u unwrappingStore) Unwrap() storage.DoltStorage { return u.DoltStorage }

// TestValidateWorkspaceIdentity_SkipsReadWhenStoreVerifiedAtOpen covers the
// bd-7c0u4 rank 4 cut: a store whose open verified identity for this beads
// directory is not queried again, and every other store is checked exactly
// as before, mismatch refused.
func TestValidateWorkspaceIdentity_SkipsReadWhenStoreVerifiedAtOpen(t *testing.T) {
	const localID = "11111111-1111-1111-1111-111111111111"
	const foreignID = "22222222-2222-2222-2222-222222222222"
	beadsDir := t.TempDir()
	if err := (&configfile.Config{ProjectID: localID}).Save(beadsDir); err != nil {
		t.Fatalf("save metadata.json: %v", err)
	}

	for _, c := range []struct {
		name      string
		build     func() (storage.DoltStorage, *identityProbeStore)
		wantReads int
		wantErr   bool
	}{
		{
			// The database id is foreign on purpose: a store that says it
			// verified is trusted and not read at all.
			name: "verified at open is not queried",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &verifyingProbeStore{identityProbeStore: identityProbeStore{dbProjectID: foreignID}, verifiedDir: beadsDir}
				return s, &s.identityProbeStore
			},
			wantReads: 0,
		},
		{
			name: "verified at open behind a decorator is not queried",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &verifyingProbeStore{identityProbeStore: identityProbeStore{dbProjectID: foreignID}, verifiedDir: beadsDir}
				return unwrappingStore{s}, &s.identityProbeStore
			},
			wantReads: 0,
		},
		{
			name: "not verified, matching id is checked and passes",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &verifyingProbeStore{identityProbeStore: identityProbeStore{dbProjectID: localID}}
				return s, &s.identityProbeStore
			},
			wantReads: 1,
		},
		{
			name: "not verified, foreign id is refused",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &verifyingProbeStore{identityProbeStore: identityProbeStore{dbProjectID: foreignID}}
				return s, &s.identityProbeStore
			},
			wantReads: 1,
			wantErr:   true,
		},
		{
			name: "verified for another beads dir, foreign id is refused",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &verifyingProbeStore{identityProbeStore: identityProbeStore{dbProjectID: foreignID}, verifiedDir: filepath.Join(beadsDir, "other")}
				return s, &s.identityProbeStore
			},
			wantReads: 1,
			wantErr:   true,
		},
		{
			name: "store without the interface, foreign id is refused",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &identityProbeStore{dbProjectID: foreignID}
				return s, s
			},
			wantReads: 1,
			wantErr:   true,
		},
		{
			name: "store without the interface, matching id passes",
			build: func() (storage.DoltStorage, *identityProbeStore) {
				s := &identityProbeStore{dbProjectID: localID}
				return s, s
			},
			wantReads: 1,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, probe := c.build()
			origStore := store
			store = st
			t.Cleanup(func() { store = origStore })

			err := validateWorkspaceIdentity(context.Background(), beadsDir)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateWorkspaceIdentity err = %v, want error: %v", err, c.wantErr)
			}
			if probe.metadataReads != c.wantReads {
				t.Errorf("GetMetadata calls = %d, want %d", probe.metadataReads, c.wantReads)
			}
		})
	}
}
