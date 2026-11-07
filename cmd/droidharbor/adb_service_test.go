package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestImportADBAuthSeedPopulatesEmptyProfileAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	seed := filepath.Join(root, "seed")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(seed, 0700); err != nil {
		t.Fatal(err)
	}
	privateKey := []byte("private authorization material")
	if err := os.Mkdir(filepath.Join(profile, ".android"), 0700); err != nil {
		t.Fatal(err)
	}
	publicKey := []byte("public authorization material")
	for name, contents := range map[string][]byte{"adbkey": privateKey, "adbkey.pub": publicKey} {
		if err := os.WriteFile(filepath.Join(seed, name), contents, 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := importADBAuthSeed(profile, seed); err != nil {
		t.Fatalf("import into absent target failed: %v", err)
	}
	if err := importADBAuthSeed(profile, seed); err != nil {
		t.Fatalf("identical repeat import failed: %v", err)
	}
	for name, expected := range map[string][]byte{"adbkey": privateKey, "adbkey.pub": publicKey} {
		actual, err := os.ReadFile(filepath.Join(profile, ".android", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Errorf("imported %s differs from seed", name)
		}
	}
}

func TestImportADBAuthSeedRefusesAndPreservesDifferentProfileKeys(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile")
	android := filepath.Join(profile, ".android")
	seed := filepath.Join(root, "seed")
	for _, directory := range []string{profile, android, seed} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	seedKey := []byte("backup private key")
	existingKey := []byte("existing private key")
	if err := os.WriteFile(filepath.Join(seed, "adbkey"), seedKey, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(android, "adbkey"), existingKey, 0600); err != nil {
		t.Fatal(err)
	}

	if err := importADBAuthSeed(profile, seed); err == nil {
		t.Fatal("expected different existing key to be rejected")
	}
	actual, err := os.ReadFile(filepath.Join(android, "adbkey"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, existingKey) {
		t.Fatal("refused import modified the existing private key")
	}
}
