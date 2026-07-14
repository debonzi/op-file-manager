package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/debonzi/op-file-manager/internal/opclient"
)

type fakeVaultManager struct {
	createdName string
	listCalls   int
}

func (f *fakeVaultManager) ListVaults(context.Context) ([]opclient.Vault, error) {
	f.listCalls++
	return []opclient.Vault{{ID: "existing", Name: "Existing"}, {ID: "new", Name: "New project"}}, nil
}

func (f *fakeVaultManager) CreateVault(_ context.Context, name string) (opclient.Vault, error) {
	f.createdName = name
	return opclient.Vault{ID: "new", Name: name}, nil
}

func TestChooseVaultCanCreateAndThenSelectVault(t *testing.T) {
	manager := &fakeVaultManager{}
	input := strings.NewReader("n\nNew project\n2\n")
	var output bytes.Buffer
	vault, err := chooseVault(context.Background(), manager, nil, input, &output)
	if err != nil {
		t.Fatal(err)
	}
	if manager.createdName != "New project" {
		t.Fatalf("CreateVault() name = %q", manager.createdName)
	}
	if manager.listCalls != 1 {
		t.Fatalf("ListVaults() calls = %d, want 1 refresh", manager.listCalls)
	}
	if vault.ID != "new" {
		t.Fatalf("selected vault = %#v", vault)
	}
	if !strings.Contains(output.String(), "Created vault") {
		t.Fatalf("wizard output did not report vault creation: %q", output.String())
	}
}
