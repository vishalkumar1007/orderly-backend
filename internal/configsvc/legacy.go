package configsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/orderly/orderly-backend/db/sqlc"
	"github.com/orderly/orderly-backend/internal/secretbox"
)

// legacySMTP is the shape the pre-configuration-system SMTP settings were
// stored in, inside platform_settings.config. The password in that blob was
// plaintext, which is what this migration exists to remove.
type legacySMTP struct {
	Enabled    bool   `json:"enabled"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	FromName   string `json:"from_name"`
	FromEmail  string `json:"from_email"`
	Encryption string `json:"encryption"`
}

// legacySettings is the whole stored platform settings blob, of which only the
// smtp member is of interest here.
type legacySettings struct {
	SMTP legacySMTP `json:"smtp"`
}

// MigrateLegacySMTP moves a pre-existing plaintext SMTP configuration into the
// encrypted platform configuration store and then removes the password from the
// legacy blob.
//
// It is idempotent and safe to run on every boot:
//
//   - it does nothing when there is no legacy SMTP configuration;
//   - it does nothing when a platform SMTP row already exists, so a re-run
//     cannot overwrite an operator's newer configuration;
//   - it refuses to run when the encryption key is missing in production, rather
//     than copying a plaintext secret into a column that claims to be sealed.
//
// The legacy blob is only rewritten once the new row is committed, so a crash
// between the two steps leaves the old value intact and the next boot retries.
func MigrateLegacySMTP(ctx context.Context, q *sqlc.Queries, box *secretbox.Box, log Logger) (bool, error) {
	row, err := q.GetPlatformSettings(ctx)
	if err != nil {
		// No settings row at all: nothing to migrate.
		return false, nil
	}
	if len(row.Config) == 0 {
		return false, nil
	}

	var legacy legacySettings
	if err := json.Unmarshal(row.Config, &legacy); err != nil {
		// A corrupt blob is not ours to fix; leave it alone and say so.
		if log != nil {
			log.Warn("legacy platform settings could not be parsed, skipping smtp migration", "error", err.Error())
		}
		return false, nil
	}

	smtp := legacy.SMTP
	hasAny := strings.TrimSpace(smtp.Host) != "" ||
		strings.TrimSpace(smtp.FromEmail) != "" ||
		strings.TrimSpace(smtp.Username) != ""
	if !hasAny {
		return false, nil
	}
	if strings.TrimSpace(smtp.Password) == "" && !smtp.Enabled {
		// Only ever saved in the default shape; nothing worth carrying over.
		return false, nil
	}

	// Never overwrite an existing platform configuration.
	if _, err := q.GetPlatformConfiguration(ctx, string(ServiceSMTP)); err == nil {
		if log != nil {
			log.Info("platform smtp configuration already exists, skipping legacy migration")
		}
		return false, nil
	}

	if box.Disabled() {
		// Copying a plaintext secret into a column named secret_config would be
		// worse than leaving it where it is, because the new schema implies it
		// is protected. Stop and let the operator set a key.
		return false, fmt.Errorf(
			"legacy smtp password found in platform_settings but CONFIG_ENCRYPTION_KEY is not set; " +
				"set it before migrating so the secret is not re-stored in plaintext")
	}

	cfg := SMTPConfig{
		Provider:   ProviderSMTP,
		Host:       smtp.Host,
		Port:       smtp.Port,
		Encryption: smtp.Encryption,
		Username:   smtp.Username,
		FromName:   smtp.FromName,
		FromEmail:  smtp.FromEmail,
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		if log != nil {
			log.Warn("legacy smtp configuration is incomplete, skipping migration", "reason", err.Error())
		}
		return false, nil
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	config := map[string]any{}
	if err := json.Unmarshal(raw, &config); err != nil {
		return false, err
	}

	secrets := map[string]string{}
	if strings.TrimSpace(smtp.Password) != "" {
		sealed, err := box.SealString(smtp.Password, platformContext(ServiceSMTP))
		if err != nil {
			return false, err
		}
		secrets["password"] = sealed
	}
	secretRaw, err := json.Marshal(secrets)
	if err != nil {
		return false, err
	}

	if _, err := q.UpsertPlatformConfiguration(ctx, sqlc.UpsertPlatformConfigurationParams{
		ServiceType:  string(ServiceSMTP),
		Provider:     ProviderSMTP,
		Config:       raw,
		SecretConfig: secretRaw,
		Status:       string(deriveStatus(smtp.Enabled, secrets, StatusUnconfigured)),
		Enabled:      smtp.Enabled,
	}); err != nil {
		return false, fmt.Errorf("write migrated platform smtp configuration: %w", err)
	}

	// Only now remove the plaintext, so a failure above leaves the original
	// recoverable.
	scrubbed, err := scrubLegacyPassword(row.Config)
	if err != nil {
		return true, fmt.Errorf("smtp configuration migrated but the legacy password could not be removed: %w", err)
	}
	if _, err := q.UpsertPlatformSettings(ctx, scrubbed); err != nil {
		return true, fmt.Errorf("smtp configuration migrated but scrubbing the legacy blob failed: %w", err)
	}

	if log != nil {
		log.Info("migrated legacy platform smtp configuration into the encrypted store",
			"host", smtp.Host, "had_password", strings.TrimSpace(smtp.Password) != "")
	}
	return true, nil
}

// scrubLegacyPassword returns a copy of the settings blob with the smtp
// password removed, leaving every other key intact.
func scrubLegacyPassword(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	smtpRaw, ok := doc["smtp"]
	if !ok {
		return raw, nil
	}
	var smtp map[string]json.RawMessage
	if err := json.Unmarshal(smtpRaw, &smtp); err != nil {
		return nil, err
	}
	if _, had := smtp["password"]; !had {
		return raw, nil
	}
	delete(smtp, "password")

	// Record that a secret was removed so the operator is not confused by the
	// now-absent field, and so it is obvious the value moved rather than lost.
	note, _ := json.Marshal("removed: managed in Settings > Email")
	smtp["password_migrated"] = note

	cleanSMTP, err := json.Marshal(smtp)
	if err != nil {
		return nil, err
	}
	doc["smtp"] = cleanSMTP
	return json.Marshal(doc)
}
