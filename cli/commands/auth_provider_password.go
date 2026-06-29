package commands

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"miren.dev/runtime/api/ingress"
	"miren.dev/runtime/api/ingress/ingress_v1alpha"
	"miren.dev/runtime/pkg/ui"
)

func AuthProviderAddPassword(ctx *Context, opts struct {
	Name     string `position:"0" usage:"Name for this password provider" required:"true"`
	Password string `long:"password" description:"Password (omit to prompt interactively, use @file to read from file)"`
	Update   bool   `long:"update" description:"Overwrite an existing provider with the same name (rotates password)"`
	ConfigCentric
}) error {
	if opts.Name == "" {
		return fmt.Errorf("provider name is required")
	}

	password := opts.Password

	switch {
	case password == "":
		prompted, err := ui.PromptForInput(
			ui.WithLabel("Enter password"),
			ui.WithSensitive(true),
		)
		if err != nil {
			return fmt.Errorf("failed to read password: %w", err)
		}
		if prompted == "" {
			return fmt.Errorf("password cannot be empty")
		}
		password = prompted
	case strings.HasPrefix(password, "@"):
		data, err := os.ReadFile(password[1:])
		if err != nil {
			return fmt.Errorf("failed to read password from file %s: %w", password[1:], err)
		}
		password = strings.TrimRight(string(data), "\r\n")
		if password == "" {
			return fmt.Errorf("password file is empty")
		}
	}

	client, err := ctx.RPCClient("entities")
	if err != nil {
		return err
	}

	ic := ingress.NewClient(ctx.Log, client)

	existing, err := ic.GetPasswordProvider(ctx, opts.Name)
	if err != nil {
		return fmt.Errorf("failed to check for existing password provider: %w", err)
	}
	if existing != nil && !opts.Update {
		return fmt.Errorf("password provider %q already exists. Pass --update to overwrite (rotates password)", opts.Name)
	}

	oidcExisting, err := ic.GetOIDCProvider(ctx, opts.Name)
	if err != nil {
		return fmt.Errorf("failed to check for existing OIDC provider: %w", err)
	}
	if oidcExisting != nil {
		return fmt.Errorf("an OIDC provider named %q already exists. Provider names must be unique across types", opts.Name)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	provider := &ingress_v1alpha.PasswordProvider{
		Name:         opts.Name,
		PasswordHash: string(hash),
	}

	_, err = ic.CreateOrUpdatePasswordProvider(ctx, provider)
	if err != nil {
		return fmt.Errorf("failed to create password provider: %w", err)
	}

	items := []ui.NamedValue{
		ui.NewNamedValue("Name", opts.Name),
		ui.NewNamedValue("Type", "password"),
	}

	ctx.Printf("%s\n", ui.NewNamedValueList(items).Render())
	return nil
}
