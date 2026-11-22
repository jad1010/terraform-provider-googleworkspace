// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/hashicorp/go-cty/cty"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
)

var gmailUserSettingsBlocks = []string{"imap", "pop", "auto_forwarding"}

var gmailMessageDispositions = []string{"leaveInInbox", "archive", "trash", "markRead"}

// newGmailUserSettingsService creates the Gmail service that impersonates the
// mailbox owner. Unit tests replace it to send the requests to a fake Gmail API.
var newGmailUserSettingsService = func(ctx context.Context, client *apiClient, userEmail string) (*gmail.Service, diag.Diagnostics) {
	return client.NewGmailService(ctx, userEmail)
}

func resourceGmailUserSettings() *schema.Resource {
	return &schema.Resource{
		Description: "Gmail User Settings resource manages the IMAP, POP and automatic forwarding settings of one " +
			"Gmail user mailbox. Only the setting blocks that are in the configuration are managed. " +
			"The provider impersonates the mailbox owner with domain-wide delegation. " +
			"The IMAP and POP settings need the `https://www.googleapis.com/auth/gmail.settings.basic` client scope. " +
			"The automatic forwarding setting also needs the `https://www.googleapis.com/auth/gmail.settings.sharing` " +
			"client scope. Both scopes are in the default `oauth_scopes` of the provider. " +
			"Gmail settings cannot be deleted: a destroy only removes the resource from the Terraform state " +
			"and does not change the mailbox. An import reads all three setting blocks. If the configuration " +
			"does not have a block, the next apply removes that block from the state and does not change the mailbox. " +
			"If the provider credentials cannot impersonate the user, Gmail rejects the requests and the mailbox is not changed. " +
			"If a refresh finds that the user does not exist (the token exchange fails with `invalid_grant` and " +
			"\"Invalid email or User ID\"), the resource is removed from the state. This covers service account " +
			"key credentials and `access_token` with `service_account`. Other errors, including a 404 from Gmail, " +
			"fail the refresh and keep the resource in the state.",

		CreateContext: resourceGmailUserSettingsCreate,
		ReadContext:   resourceGmailUserSettingsRead,
		UpdateContext: resourceGmailUserSettingsUpdate,
		DeleteContext: resourceGmailUserSettingsDelete,

		Importer: &schema.ResourceImporter{
			StateContext: resourceGmailUserSettingsImport,
		},

		CustomizeDiff: resourceGmailUserSettingsCustomizeDiff,

		Schema: map[string]*schema.Schema{
			"user_email": {
				Description:      "The primary email address of the mailbox owner. The provider impersonates this user.",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				DiffSuppressFunc: diffSuppressCaseInsensitive,
				// An address without "@", for example "me", could address the
				// mailbox of the authenticated user instead.
				ValidateDiagFunc: validation.ToDiagFunc(validation.StringMatch(
					regexp.MustCompile(`^[^@\s]+@[^@\s]+$`), "must be an email address")),
			},
			"imap": {
				Description:  "The IMAP settings of the mailbox. If this block is not set, the IMAP settings are not managed.",
				Type:         schema.TypeList,
				Optional:     true,
				MaxItems:     1,
				AtLeastOneOf: gmailUserSettingsBlocks,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"enabled": {
							Description: "Whether IMAP is enabled for the mailbox.",
							Type:        schema.TypeBool,
							Required:    true,
						},
						"auto_expunge": {
							Description: "If `true`, Gmail immediately expunges a message when it is marked as deleted in IMAP. " +
								"If `false`, Gmail waits for an update from the client before it expunges messages marked as deleted. " +
								"If not set, the current value is kept.",
							Type:     schema.TypeBool,
							Optional: true,
							Computed: true,
						},
						"expunge_behavior": {
							Description: "The action on a message that is marked as deleted and expunged from the last visible IMAP folder. " +
								"One of `archive`, `trash` or `deleteForever`. If not set, the current value is kept.",
							Type:             schema.TypeString,
							Optional:         true,
							Computed:         true,
							ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{"archive", "trash", "deleteForever"}, false)),
						},
						"max_folder_size": {
							Description: "The maximum number of messages that an IMAP folder can contain. " +
								"One of `0`, `1000`, `2000`, `5000` or `10000`. `0` means no limit. If not set, the current value is kept.",
							Type:             schema.TypeInt,
							Optional:         true,
							Computed:         true,
							ValidateDiagFunc: validation.ToDiagFunc(validation.IntInSlice([]int{0, 1000, 2000, 5000, 10000})),
						},
					},
				},
			},
			"pop": {
				Description:  "The POP settings of the mailbox. If this block is not set, the POP settings are not managed.",
				Type:         schema.TypeList,
				Optional:     true,
				MaxItems:     1,
				AtLeastOneOf: gmailUserSettingsBlocks,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"access_window": {
							Description: "The range of messages that are accessible with POP. " +
								"One of `disabled`, `fromNowOn` or `allMail`. Not verified: after a POP client fetches mail, " +
								"Gmail can possibly read back `allMail` as a different value, and the plan then shows a change.",
							Type:             schema.TypeString,
							Required:         true,
							ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice([]string{"disabled", "fromNowOn", "allMail"}, false)),
						},
						"disposition": {
							Description: "The action on a message after it is fetched with POP. " +
								"One of `leaveInInbox`, `archive`, `trash` or `markRead`. If not set, the current value is kept.",
							Type:             schema.TypeString,
							Optional:         true,
							Computed:         true,
							ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice(gmailMessageDispositions, false)),
						},
					},
				},
			},
			"auto_forwarding": {
				Description: "The automatic forwarding settings of the mailbox. " +
					"If this block is not set, the automatic forwarding settings are not managed. " +
					"This is the setting of one mailbox, not the admin setting that controls whether users can " +
					"forward mail. `enabled = false` does not stop forwarding by Gmail filters.",
				Type:         schema.TypeList,
				Optional:     true,
				MaxItems:     1,
				AtLeastOneOf: gmailUserSettingsBlocks,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"enabled": {
							Description: "Whether Gmail automatically forwards all incoming mail to `email_address`.",
							Type:        schema.TypeBool,
							Required:    true,
						},
						"email_address": {
							Description: "The email address to which Gmail forwards all incoming mail. Required if `enabled` is `true`. " +
								"The address must be a forwarding address of the mailbox with verification status `accepted`. " +
								"Create it outside Terraform; addresses in the same domain may not require verification. " +
								"If `enabled` is `false`, the provider does not read this value back from the API.",
							Type:             schema.TypeString,
							Optional:         true,
							DiffSuppressFunc: diffSuppressCaseInsensitive,
						},
						"disposition": {
							Description: "The state of a message after Gmail forwards it. " +
								"One of `leaveInInbox`, `archive`, `trash` or `markRead`. If not set, the current value is kept.",
							Type:             schema.TypeString,
							Optional:         true,
							Computed:         true,
							ValidateDiagFunc: validation.ToDiagFunc(validation.StringInSlice(gmailMessageDispositions, false)),
						},
					},
				},
			},
			// Adding a computed id simply to override the `optional` id that gets added in the SDK
			// that will then display improperly in the docs
			"id": {
				Description: "The ID of this resource. It is the same as `user_email`.",
				Type:        schema.TypeString,
				Computed:    true,
			},
		},
	}
}

func diffSuppressCaseInsensitive(_, old, new string, _ *schema.ResourceData) bool {
	return strings.EqualFold(old, new)
}

// resourceGmailUserSettingsCustomizeDiff rejects automatic forwarding that is
// enabled without a forwarding address. The API also rejects it, but only at
// apply time.
func resourceGmailUserSettingsCustomizeDiff(_ context.Context, d *schema.ResourceDiff, _ interface{}) error {
	if len(d.Get("auto_forwarding").([]interface{})) == 0 {
		return nil
	}
	if !d.NewValueKnown("auto_forwarding.0.enabled") || !d.NewValueKnown("auto_forwarding.0.email_address") {
		return nil
	}
	if d.Get("auto_forwarding.0.enabled").(bool) && d.Get("auto_forwarding.0.email_address").(string) == "" {
		return fmt.Errorf("auto_forwarding.0.email_address is required when auto_forwarding.0.enabled is true")
	}
	return nil
}

func resourceGmailUserSettingsCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	userEmail := d.Get("user_email").(string)
	log.Printf("[DEBUG] Creating Gmail User Settings %q", userEmail)

	diags := writeGmailUserSettings(ctx, d, meta, false)
	if diags.HasError() {
		return diags
	}

	d.SetId(userEmail)

	log.Printf("[DEBUG] Finished creating Gmail User Settings %q", d.Id())

	return resourceGmailUserSettingsRead(ctx, d, meta)
}

func resourceGmailUserSettingsUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	log.Printf("[DEBUG] Updating Gmail User Settings %q", d.Id())

	diags := writeGmailUserSettings(ctx, d, meta, true)
	if diags.HasError() {
		// Keep the prior state, so that the next plan writes the settings again.
		d.Partial(true)
		return diags
	}

	log.Printf("[DEBUG] Finished updating Gmail User Settings %q", d.Id())

	return resourceGmailUserSettingsRead(ctx, d, meta)
}

// resourceGmailUserSettingsRead refreshes only the blocks that are in the
// state. A block that is not in the configuration is not in the state, so the
// settings that it would manage cause no drift. The importer reads all blocks.
func resourceGmailUserSettingsRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	var blocks []string
	for _, block := range gmailUserSettingsBlocks {
		if len(d.Get(block).([]interface{})) > 0 {
			blocks = append(blocks, block)
		}
	}

	return readGmailUserSettings(ctx, d, meta, blocks)
}

// resourceGmailUserSettingsDelete only removes the resource from the state.
// Gmail settings cannot be deleted, and a destroy must not change the mailbox.
func resourceGmailUserSettingsDelete(_ context.Context, d *schema.ResourceData, _ interface{}) diag.Diagnostics {
	log.Printf("[DEBUG] Removing Gmail User Settings %q from state; the mailbox settings are not changed", d.Id())
	d.SetId("")
	return nil
}

func resourceGmailUserSettingsImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	userEmail := d.Id()
	if userEmail == "" || !strings.Contains(userEmail, "@") {
		return nil, fmt.Errorf("Unexpected format of ID (%q), expected the email address of the mailbox owner", userEmail)
	}
	if err := d.Set("user_email", userEmail); err != nil {
		return nil, err
	}

	diags := readGmailUserSettings(ctx, d, meta, gmailUserSettingsBlocks)
	if diags.HasError() {
		for _, diagnostic := range diags {
			if diagnostic.Severity == diag.Error {
				return nil, fmt.Errorf("%s: %s", diagnostic.Summary, diagnostic.Detail)
			}
		}
	}
	if d.Id() == "" {
		return nil, fmt.Errorf("Gmail User Settings %q not found", userEmail)
	}

	return []*schema.ResourceData{d}, nil
}

func readGmailUserSettings(ctx context.Context, d *schema.ResourceData, meta interface{}, blocks []string) diag.Diagnostics {
	if len(blocks) == 0 {
		return nil
	}

	settingsService, diags := getGmailUserSettingsService(ctx, d, meta)
	if diags.HasError() {
		return diags
	}

	// Use the email address, not "me": if the credentials cannot impersonate
	// the user, "me" is the authenticated user and Gmail would return the
	// settings of a different mailbox. With the email address, Gmail rejects
	// the request.
	userID := d.Get("user_email").(string)

	log.Printf("[DEBUG] Getting Gmail User Settings %q (%s)", d.Id(), strings.Join(blocks, ", "))

	for _, block := range blocks {
		var err error
		switch block {
		case "imap":
			var imap *gmail.ImapSettings
			err = retryNotFound(ctx, func() (e error) {
				imap, e = settingsService.GetImap(userID).Context(ctx).Do()
				return
			})
			if err == nil {
				err = d.Set("imap", flattenGmailImapSettings(imap, d))
			}
		case "pop":
			var pop *gmail.PopSettings
			err = retryNotFound(ctx, func() (e error) {
				pop, e = settingsService.GetPop(userID).Context(ctx).Do()
				return
			})
			if err == nil {
				err = d.Set("pop", flattenGmailPopSettings(pop, d))
			}
		case "auto_forwarding":
			var autoForwarding *gmail.AutoForwarding
			err = retryNotFound(ctx, func() (e error) {
				autoForwarding, e = settingsService.GetAutoForwarding(userID).Context(ctx).Do()
				return
			})
			if err == nil {
				err = d.Set("auto_forwarding", flattenGmailAutoForwarding(autoForwarding, d))
			}
		}
		if err != nil {
			if isGmailUserNotFoundTokenError(err) {
				log.Printf("[WARN] Removing Gmail User Settings %q because the user does not exist: %s", d.Id(), err)
				d.SetId("")
				return diag.Diagnostics{{
					Severity: diag.Warning,
					Summary:  fmt.Sprintf("Gmail user %q does not exist", userID),
					Detail: "The token exchange for the user failed with invalid_grant (" + err.Error() + "). " +
						"The resource is removed from the state.",
				}}
			}
			// The settings of an existing mailbox do not return 404, so a 404
			// is an error, not a resource that is gone. Removing the resource
			// from the state would silently stop managing the settings.
			if isApiErrorWithCode(err, 404) {
				return diag.Errorf("Error when reading the %s settings of Gmail user %q: Gmail returned 404 (%s). "+
					"Check that the user exists and has a Gmail license. If the user was deleted, remove the resource "+
					"from the configuration and the state.", block, userID, err.Error())
			}
			return diag.Errorf("Error when reading the %s settings of Gmail user %q: %s", block, userID, err.Error())
		}
	}

	log.Printf("[DEBUG] Finished getting Gmail User Settings %q", d.Id())

	return nil
}

// writeGmailUserSettings writes the blocks that are in the configuration. On
// update, it writes only the blocks that changed. A block that was removed
// from the configuration is not written: the mailbox keeps its settings.
//
// The update methods replace the whole setting, so each request starts from
// the current setting and changes only the values that are in the
// configuration. An optional value that is not in the configuration keeps its
// current value in the mailbox.
func writeGmailUserSettings(ctx context.Context, d *schema.ResourceData, meta interface{}, update bool) diag.Diagnostics {
	var blocks []string
	for _, block := range gmailUserSettingsBlocks {
		if len(d.Get(block).([]interface{})) == 0 {
			continue
		}
		if update && !d.HasChange(block) {
			continue
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil
	}

	// Terraform sends the configuration with each apply. Without it, a value
	// that is not in the configuration cannot be told from a planned value.
	if d.GetRawConfig().IsNull() {
		return diag.Errorf("Error when writing the settings of Gmail user %q: the resource configuration is not available", d.Get("user_email").(string))
	}

	settingsService, diags := getGmailUserSettingsService(ctx, d, meta)
	if diags.HasError() {
		return diags
	}

	// See readGmailUserSettings for why the email address is used, not "me".
	userID := d.Get("user_email").(string)

	for _, block := range blocks {
		log.Printf("[DEBUG] Writing Gmail User Settings %q block %q", userID, block)

		var err error
		switch block {
		case "imap":
			var imap *gmail.ImapSettings
			imap, err = settingsService.GetImap(userID).Context(ctx).Do()
			if err == nil {
				// Do not send back an unspecified value that is not in the configuration.
				imap.ExpungeBehavior = withoutUnspecified(imap.ExpungeBehavior)
				imap.Enabled = d.Get("imap.0.enabled").(bool)
				if gmailSettingConfigured(d, "imap", "auto_expunge") {
					imap.AutoExpunge = d.Get("imap.0.auto_expunge").(bool)
				}
				if gmailSettingConfigured(d, "imap", "expunge_behavior") {
					imap.ExpungeBehavior = d.Get("imap.0.expunge_behavior").(string)
				}
				if gmailSettingConfigured(d, "imap", "max_folder_size") {
					imap.MaxFolderSize = int64(d.Get("imap.0.max_folder_size").(int))
				}
				imap.ForceSendFields = []string{"Enabled", "AutoExpunge", "MaxFolderSize"}
				_, err = settingsService.UpdateImap(userID, imap).Context(ctx).Do()
			}
		case "pop":
			var pop *gmail.PopSettings
			pop, err = settingsService.GetPop(userID).Context(ctx).Do()
			if err == nil {
				pop.Disposition = withoutUnspecified(pop.Disposition)
				pop.AccessWindow = d.Get("pop.0.access_window").(string)
				if gmailSettingConfigured(d, "pop", "disposition") {
					pop.Disposition = d.Get("pop.0.disposition").(string)
				}
				_, err = settingsService.UpdatePop(userID, pop).Context(ctx).Do()
			}
		case "auto_forwarding":
			var autoForwarding *gmail.AutoForwarding
			autoForwarding, err = settingsService.GetAutoForwarding(userID).Context(ctx).Do()
			if err == nil {
				autoForwarding.Disposition = withoutUnspecified(autoForwarding.Disposition)
				autoForwarding.Enabled = d.Get("auto_forwarding.0.enabled").(bool)
				// The plan requires an address if forwarding is enabled. If
				// forwarding is disabled and no address is set, the request
				// has no address.
				autoForwarding.EmailAddress = d.Get("auto_forwarding.0.email_address").(string)
				if gmailSettingConfigured(d, "auto_forwarding", "disposition") {
					autoForwarding.Disposition = d.Get("auto_forwarding.0.disposition").(string)
				}
				autoForwarding.ForceSendFields = []string{"Enabled"}
				_, err = settingsService.UpdateAutoForwarding(userID, autoForwarding).Context(ctx).Do()
			}
		}
		if err != nil {
			return diag.Errorf("Error when writing the %s settings of Gmail user %q: %s", block, userID, err.Error())
		}
	}

	return nil
}

// gmailSettingConfigured reports whether the configuration sets the attribute
// attr of the block. The planned value cannot tell this: an optional and
// computed attribute that is not set has the prior state value on update.
func gmailSettingConfigured(d *schema.ResourceData, block, attr string) bool {
	blockValue := d.GetRawConfig().GetAttr(block)
	if blockValue.IsNull() || !blockValue.IsKnown() || blockValue.LengthInt() == 0 {
		return false
	}

	value := blockValue.Index(cty.NumberIntVal(0)).GetAttr(attr)
	return value.IsKnown() && !value.IsNull()
}

func getGmailUserSettingsService(ctx context.Context, d *schema.ResourceData, meta interface{}) (*gmail.UsersSettingsService, diag.Diagnostics) {
	client := meta.(*apiClient)

	gmailService, diags := newGmailUserSettingsService(ctx, client, d.Get("user_email").(string))
	if diags.HasError() {
		return nil, diags
	}

	return GetGmailUsersSettingsService(gmailService)
}

func flattenGmailImapSettings(imap *gmail.ImapSettings, d *schema.ResourceData) []interface{} {
	values := map[string]interface{}{
		"enabled":          imap.Enabled,
		"auto_expunge":     imap.AutoExpunge,
		"expunge_behavior": imap.ExpungeBehavior,
		"max_folder_size":  int(imap.MaxFolderSize),
	}
	if !imap.Enabled {
		keepPriorGmailValues(d, "imap", values, "auto_expunge", "expunge_behavior", "max_folder_size")
	}
	return []interface{}{values}
}

func flattenGmailPopSettings(pop *gmail.PopSettings, d *schema.ResourceData) []interface{} {
	values := map[string]interface{}{
		"access_window": pop.AccessWindow,
		"disposition":   pop.Disposition,
	}
	if pop.AccessWindow == "disabled" {
		keepPriorGmailValues(d, "pop", values, "disposition")
	}
	return []interface{}{values}
}

// flattenGmailAutoForwarding keeps the email address from the state when
// forwarding is disabled, because the address has no effect then and the API
// can return a previous address or no address.
func flattenGmailAutoForwarding(autoForwarding *gmail.AutoForwarding, d *schema.ResourceData) []interface{} {
	emailAddress := autoForwarding.EmailAddress
	if !autoForwarding.Enabled {
		emailAddress = ""
		if len(d.Get("auto_forwarding").([]interface{})) > 0 {
			emailAddress = d.Get("auto_forwarding.0.email_address").(string)
		}
	}
	values := map[string]interface{}{
		"enabled":       autoForwarding.Enabled,
		"email_address": emailAddress,
		"disposition":   autoForwarding.Disposition,
	}
	if !autoForwarding.Enabled {
		keepPriorGmailValues(d, "auto_forwarding", values, "disposition")
	}
	return []interface{}{values}
}

// keepPriorGmailValues replaces an empty or unspecified API value with the
// value in the state or plan. The caller uses it only when the feature is
// disabled: the API can then omit the values, which have no effect, and the
// configured values would never converge. A real change to the zero value
// while the feature is disabled is not detected.
func keepPriorGmailValues(d *schema.ResourceData, block string, values map[string]interface{}, keys ...string) {
	if len(d.Get(block).([]interface{})) == 0 {
		return
	}
	for _, key := range keys {
		switch v := values[key].(type) {
		case bool:
			if v {
				continue
			}
		case int:
			if v != 0 {
				continue
			}
		case string:
			if v != "" && !strings.HasSuffix(v, "Unspecified") {
				continue
			}
		}
		values[key] = d.Get(block + ".0." + key)
	}
}

// withoutUnspecified returns "" for an enum value such as
// "dispositionUnspecified", so that an update does not send it.
func withoutUnspecified(value string) string {
	if strings.HasSuffix(value, "Unspecified") {
		return ""
	}
	return value
}

// impersonateTokenErrorPrefix starts the error of the impersonate package
// when the token endpoint rejects the token exchange. The response body
// follows the prefix.
const impersonateTokenErrorPrefix = "impersonate: status code 400: "

// isGmailUserNotFoundTokenError reports whether the token exchange for the
// impersonated user failed because the user does not exist: Google returns
// invalid_grant with the description "Invalid email or User ID". Other
// invalid_grant errors, for example a clock skew, are not matched.
//
// It covers the token sources that the provider uses:
//   - service account key credentials (golang.org/x/oauth2/jwt): an
//     oauth2.RetrieveError with the response body and no ErrorCode.
//   - access_token with service_account (google.golang.org/api/impersonate):
//     a plain error with impersonateTokenErrorPrefix and the response body.
func isGmailUserNotFoundTokenError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		if retrieveErr.ErrorCode != "" {
			return isUserNotFoundOAuth2Error(retrieveErr.ErrorCode, retrieveErr.ErrorDescription)
		}
		return isUserNotFoundOAuth2ErrorBody(retrieveErr.Body)
	}

	for e := err; e != nil; e = errors.Unwrap(e) {
		if body, ok := strings.CutPrefix(e.Error(), impersonateTokenErrorPrefix); ok {
			return isUserNotFoundOAuth2ErrorBody([]byte(body))
		}
	}
	return false
}

func isUserNotFoundOAuth2ErrorBody(body []byte) bool {
	var response struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return false
	}
	return isUserNotFoundOAuth2Error(response.Error, response.ErrorDescription)
}

func isUserNotFoundOAuth2Error(code, description string) bool {
	return code == "invalid_grant" && description == "Invalid email or User ID"
}
