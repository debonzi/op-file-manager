package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/debonzi/op-file-manager/internal/app"
	"github.com/debonzi/op-file-manager/internal/config"
	"github.com/debonzi/op-file-manager/internal/localfs"
	"github.com/debonzi/op-file-manager/internal/opclient"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "opfm:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "init" {
		return runInit(ctx, args[1:], stdin, stdout, stderr)
	}
	return runTUI(ctx, args, stdout, stderr)
}

func runInit(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("opfm init", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "configuration file path")
	opBinary := flags.String("op-bin", "op", "1Password CLI binary")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: opfm init [--config PATH]")
	}
	if *configPath == "" {
		var err error
		*configPath, err = config.DefaultPath()
		if err != nil {
			return err
		}
	}

	client := opclient.New(*opBinary)
	defer client.Close()
	if err := client.Check(ctx); err != nil {
		return err
	}
	account, err := authenticateInit(ctx, client, stdin, stdout)
	if err != nil {
		return err
	}
	vaults, err := client.ListVaults(ctx)
	if err != nil {
		return err
	}
	sort.Slice(vaults, func(i, j int) bool { return strings.ToLower(vaults[i].Name) < strings.ToLower(vaults[j].Name) })
	vault, err := chooseVault(ctx, client, vaults, stdin, stdout)
	if err != nil {
		return err
	}
	if err := config.Save(*configPath, config.Config{Version: 1, AccountID: account.ID, VaultID: vault.ID}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Saved 1Password account and vault IDs to %s\n", *configPath)
	return nil
}

func authenticateInit(ctx context.Context, client *opclient.Client, stdin io.Reader, stdout io.Writer) (opclient.Account, error) {
	account, err := client.WhoAmI(ctx)
	if err == nil {
		return account, nil
	}
	if !opclient.IsAuthenticationError(err) {
		return opclient.Account{}, err
	}

	reader := bufio.NewReader(stdin)
	fmt.Fprint(stdout, "No active 1Password session. [s]ign in to an existing account or [a]dd an account? ")
	choice, readErr := reader.ReadString('\n')
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return opclient.Account{}, fmt.Errorf("read sign-in choice: %w", readErr)
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "a", "add":
		if err := client.AddAccountAndSignIn(ctx); err != nil {
			return opclient.Account{}, err
		}
	case "", "s", "sign", "signin":
		if err := client.SignIn(ctx, ""); err != nil {
			return opclient.Account{}, err
		}
	default:
		return opclient.Account{}, errors.New("sign-in cancelled")
	}
	return client.WhoAmI(ctx)
}

type vaultManager interface {
	ListVaults(context.Context) ([]opclient.Vault, error)
	CreateVault(context.Context, string) (opclient.Vault, error)
}

func chooseVault(ctx context.Context, client vaultManager, vaults []opclient.Vault, stdin io.Reader, stdout io.Writer) (opclient.Vault, error) {
	reader := bufio.NewReader(stdin)
	for {
		fmt.Fprintln(stdout, "\nChoose the vault for opfm:")
		for index, vault := range vaults {
			fmt.Fprintf(stdout, "  %d) %s\n", index+1, vault.Name)
		}
		fmt.Fprint(stdout, "Vault number, or n to create a new vault: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return opclient.Vault{}, fmt.Errorf("read vault selection: %w", err)
		}
		choice := strings.TrimSpace(line)
		if strings.EqualFold(choice, "n") || strings.EqualFold(choice, "new") {
			fmt.Fprint(stdout, "New vault name: ")
			name, nameErr := reader.ReadString('\n')
			if nameErr != nil && !errors.Is(nameErr, io.EOF) {
				return opclient.Vault{}, fmt.Errorf("read new vault name: %w", nameErr)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				if errors.Is(nameErr, io.EOF) {
					return opclient.Vault{}, errors.New("vault creation cancelled")
				}
				fmt.Fprintln(stdout, "Vault name cannot be empty.")
				continue
			}
			created, createErr := client.CreateVault(ctx, name)
			if createErr != nil {
				return opclient.Vault{}, createErr
			}
			fmt.Fprintf(stdout, "Created vault %q. Refreshing vault list…\n", created.Name)
			vaults, err = client.ListVaults(ctx)
			if err != nil {
				return opclient.Vault{}, err
			}
			sort.Slice(vaults, func(i, j int) bool { return strings.ToLower(vaults[i].Name) < strings.ToLower(vaults[j].Name) })
			continue
		}
		selected, parseErr := strconv.Atoi(choice)
		if parseErr == nil && selected >= 1 && selected <= len(vaults) {
			return vaults[selected-1], nil
		}
		if errors.Is(err, io.EOF) {
			return opclient.Vault{}, errors.New("vault selection cancelled")
		}
		fmt.Fprintln(stdout, "Enter a number from the list.")
	}
}

func runTUI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("opfm", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "configuration file path")
	accountID := flags.String("account", "", "account ID override for this run")
	vaultID := flags.String("vault", "", "vault ID override for this run")
	opBinary := flags.String("op-bin", "op", "1Password CLI binary")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 1 {
		return errors.New("usage: opfm [ROOT] [--config PATH] [--account ID] [--vault ID]")
	}
	if *configPath == "" {
		var err error
		*configPath, err = config.DefaultPath()
		if err != nil {
			return err
		}
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *accountID != "" {
		cfg.AccountID = *accountID
	}
	if *vaultID != "" {
		cfg.VaultID = *vaultID
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	rootPath := "."
	if flags.NArg() == 1 {
		rootPath = flags.Arg(0)
	}
	root, err := localfs.NewRoot(rootPath)
	if err != nil {
		return err
	}
	client := opclient.New(*opBinary)
	defer client.Close()
	client.SetAccount(cfg.AccountID)
	if err := client.Check(ctx); err != nil {
		return err
	}
	account, err := client.WhoAmI(ctx)
	if err != nil {
		if !opclient.IsAuthenticationError(err) {
			return err
		}
		fmt.Fprintln(stderr, "No active 1Password session; starting 1Password CLI sign-in…")
		if err := client.SignIn(ctx, cfg.AccountID); err != nil {
			return err
		}
		account, err = client.WhoAmI(ctx)
		if err != nil {
			return err
		}
	}

	info := app.ContextInfo{AccountName: firstNonEmpty(account.Name, account.ID), VaultName: configuredVaultName(ctx, client, cfg.VaultID)}
	model := app.New(ctx, client, cfg, root, info)
	program := tea.NewProgram(model, tea.WithOutput(stdout))
	_, err = program.Run()
	return err
}

func configuredVaultName(ctx context.Context, client *opclient.Client, vaultID string) string {
	vaults, err := client.ListVaults(ctx)
	if err != nil {
		return vaultID
	}
	for _, vault := range vaults {
		if vault.ID == vaultID && vault.Name != "" {
			return vault.Name
		}
	}
	return vaultID
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
