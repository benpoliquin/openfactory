package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

func randomCLIIdentifier(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

// Operator commands are trusted local recovery/enrollment authority. Browser
// commands cannot invoke them, and none fabricate WebAuthn approval evidence.
func runOperator(ctx context.Context, c model.Config, db *store.Store, args []string, out, errout io.Writer) error {
	emit := func(v any) error { enc := json.NewEncoder(out); enc.SetIndent("", "  "); return enc.Encode(v) }
	usage := "usage: operator create ID --label LABEL --project PROJECT [--project PROJECT...] | status [ID] | pair ID [--output FILE] | pairing PAIRING_ID | confirm PAIRING_ID --fingerprint SHA256 | devices ID | revoke-device CREDENTIAL_ID | revoke ID | auth-generation | restore-auth --confirm invalidate-all-browser-auth\nRestore revokes ALL existing credentials and browser authentication; fresh trusted CLI pairing is required. Confirm uses only the fingerprint from successful phone registration; mismatch revokes the pairing."
	if len(args) == 0 {
		return errors.New(usage)
	}
	command, args := args[0], args[1:]
	switch command {
	case "create":
		if len(args) < 5 {
			return errors.New(usage)
		}
		operator := model.Operator{ID: args[0], Enabled: true}
		seen := map[string]bool{}
		for opts := args[1:]; len(opts) > 0; opts = opts[2:] {
			if len(opts) < 2 || strings.HasPrefix(opts[1], "--") {
				return errors.New("operator create options require values")
			}
			switch opts[0] {
			case "--label":
				if operator.Label != "" {
					return errors.New("duplicate --label")
				}
				operator.Label = opts[1]
			case "--project":
				p, ok := c.Projects[opts[1]]
				if !ok || !p.Trusted {
					return errors.New("operator scope requires an explicitly enrolled trusted project")
				}
				if seen[opts[1]] {
					return errors.New("duplicate --project")
				}
				seen[opts[1]] = true
				operator.ProjectScope = append(operator.ProjectScope, opts[1])
			default:
				return errors.New("unknown operator create option")
			}
		}
		if operator.Label == "" || len(operator.ProjectScope) == 0 {
			return errors.New("operator create requires --label and explicit --project scope")
		}
		sort.Strings(operator.ProjectScope)
		operator.UserHandle = make([]byte, 32)
		if _, err := rand.Read(operator.UserHandle); err != nil {
			return err
		}
		if err := db.CreateOperator(ctx, operator); err != nil {
			return err
		}
		v, err := db.GetOperator(ctx, operator.ID)
		if err != nil {
			return err
		}
		return emit(v)
	case "status":
		if len(args) > 1 {
			return errors.New("usage: operator status [ID]")
		}
		if len(args) == 1 {
			v, err := db.GetOperator(ctx, args[0])
			if err != nil {
				return err
			}
			return emit(v)
		}
		v, err := db.ListOperators(ctx)
		if err != nil {
			return err
		}
		return emit(v)
	case "pair":
		if len(args) != 1 && (len(args) != 3 || args[1] != "--output" || args[2] == "") {
			return errors.New("usage: operator pair ID [--output FILE]")
		}
		if !c.Intake.Enabled {
			return errors.New("operator pairing requires intake.enabled and an exact external origin/RP ID")
		}
		operator, err := db.GetOperator(ctx, args[0])
		if err != nil {
			return err
		}
		if !operator.Enabled || operator.RevokedAt != nil {
			return errors.New("operator is revoked or disabled")
		}
		for _, project := range operator.ProjectScope {
			if p, ok := c.Projects[project]; !ok || !p.Trusted {
				return errors.New("operator scope contains a project no longer enrolled")
			}
		}
		id, err := randomCLIIdentifier("pair-")
		if err != nil {
			return err
		}
		var bytes [32]byte
		if _, err = rand.Read(bytes[:]); err != nil {
			return err
		}
		secret := base64.RawURLEncoding.EncodeToString(bytes[:])
		// HTTP receives the encoded secret, so hash those exact bytes, not the
		// decoded entropy. Only its digest is persisted in SQLite.
		digest := sha256.Sum256([]byte(secret))
		pairing := model.OperatorPairing{ID: id, OperatorID: operator.ID, ProjectScope: append([]string(nil), operator.ProjectScope...), MaxCredentials: c.Intake.MaxCredentials, SecretHash: hex.EncodeToString(digest[:]), State: "issued", ExpiresAt: time.Now().UTC().Add(time.Duration(c.Intake.PairingTTLSeconds) * time.Second)}
		var destination *os.File
		if len(args) == 3 {
			destination, err = os.OpenFile(args[2], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return fmt.Errorf("create exclusive private pairing output: %w", err)
			}
			defer destination.Close()
		}
		if err = db.CreatePairing(ctx, pairing); err != nil {
			if destination != nil {
				os.Remove(args[2])
			}
			return err
		}
		result := struct {
			PairingID  string    `json:"pairing_id"`
			URL        string    `json:"url"`
			ExpiresAt  time.Time `json:"expires_at"`
			TTLSeconds int       `json:"ttl_seconds"`
		}{id, c.Intake.ExternalOrigin + "/plugins/openfactory/intake/pair#pairing=" + secret, pairing.ExpiresAt, c.Intake.PairingTTLSeconds}
		if destination != nil {
			if err = json.NewEncoder(destination).Encode(result); err != nil {
				return err
			}
			if err = destination.Sync(); err != nil {
				return err
			}
			return emit(map[string]any{"pairing_id": id, "output": args[2], "expires_at": pairing.ExpiresAt})
		}
		return emit(result)
	case "pairing":
		if len(args) != 1 {
			return errors.New("usage: operator pairing PAIRING_ID")
		}
		v, err := db.GetPairing(ctx, args[0])
		if err != nil {
			return err
		}
		// Confirmation must come from the successful phone registration screen,
		// never a CLI echo that can be blindly copied back into confirm.
		return emit(map[string]any{"pairing_id": v.ID, "operator_id": v.OperatorID, "project_scope": v.ProjectScope, "state": v.State, "expires_at": v.ExpiresAt})
	case "confirm":
		if len(args) != 3 || args[1] != "--fingerprint" || len(args[2]) != 64 || strings.Trim(args[2], "0123456789abcdef") != "" {
			return errors.New("usage: operator confirm PAIRING_ID --fingerprint EXACT_LOWERCASE_SHA256")
		}
		v, err := db.ConfirmPairing(ctx, args[0], args[2])
		if err != nil {
			return err
		}
		return emit(publicCredential(v))
	case "devices":
		if len(args) != 1 {
			return errors.New("usage: operator devices ID")
		}
		v, err := db.ListOperatorCredentials(ctx, args[0])
		if err != nil {
			return err
		}
		items := make([]any, 0, len(v))
		for _, credential := range v {
			items = append(items, publicCredential(credential))
		}
		return emit(items)
	case "revoke-device":
		if len(args) != 1 {
			return errors.New("usage: operator revoke-device CREDENTIAL_ID")
		}
		if err := db.RevokeCredential(ctx, args[0]); err != nil {
			return err
		}
		return emit(map[string]any{"credential_id": args[0], "revoked": true})
	case "revoke":
		if len(args) != 1 {
			return errors.New("usage: operator revoke ID")
		}
		if err := db.RevokeOperator(ctx, args[0]); err != nil {
			return err
		}
		return emit(map[string]any{"operator_id": args[0], "revoked": true})
	case "auth-generation":
		if len(args) != 0 {
			return errors.New("usage: operator auth-generation")
		}
		v, err := db.AuthGeneration(ctx)
		if err != nil {
			return err
		}
		return emit(map[string]string{"auth_generation": v})
	case "restore-auth":
		if len(args) != 2 || args[0] != "--confirm" || args[1] != "invalidate-all-browser-auth" {
			return errors.New("usage: operator restore-auth --confirm invalidate-all-browser-auth")
		}
		v, err := db.RotateAuthGeneration(ctx)
		if err != nil {
			return err
		}
		return emit(map[string]any{"auth_generation": v, "ephemeral_auth_invalidated": true, "existing_credentials_revoked": true, "fresh_cli_pairing_required": true, "approved_work_revoked": false})
	default:
		return errors.New(usage)
	}
}

func publicCredential(c model.OperatorCredential) any {
	return struct {
		ID          string     `json:"id"`
		OperatorID  string     `json:"operator_id"`
		Fingerprint string     `json:"fingerprint"`
		CreatedAt   time.Time  `json:"created_at"`
		ExpiresAt   time.Time  `json:"expires_at"`
		RevokedAt   *time.Time `json:"revoked_at,omitempty"`
		LastUsedAt  time.Time  `json:"last_used_at"`
	}{c.ID, c.OperatorID, c.Fingerprint, c.CreatedAt, c.ExpiresAt, c.RevokedAt, c.LastUsedAt}
}
