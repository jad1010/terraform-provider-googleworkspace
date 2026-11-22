## 0.13.0 (November 22, 2026)

BREAKING CHANGES:

* directory: `recovery_email` and `recovery_phone` are now sensitive in the `googleworkspace_user` resource and in the `googleworkspace_user` data source. Terraform hides their values in plan and apply output. A root module output that refers to these values now needs `sensitive = true`. To use a value where Terraform does not accept sensitive values, such as `for_each` or `count`, wrap it in `nonsensitive()`
* chromepolicy: `googleworkspace_chrome_policy` now validates values at plan time, and before an update resets the old policies. When any policy of a resource changes, the plan validates all policies of that resource, as the apply does. A plan with no change to the policies does not validate them. Validation is stricter: integers must be whole numbers in the range of their type, unsigned integers cannot be negative, `TYPE_FLOAT` values must be in the float32 range, enum numbers must be whole numbers, a value is checked only against its own field (the field in the message of the policy, or else the first top-level field with that name), each element of a repeated field is checked, a map field must be a JSON object or a list of `key` and `value` objects, a repeated field is a map field only if its entry message has the protoc name for the field (for example `LabelsEntry` for `labels`), and values in nested messages are checked against the schema. A nested type or field that is not in the schema is not checked. The plan reports all values that are not valid, not only the first one
* chromepolicy: `googleworkspace_chrome_policy` now returns an error if the same `schema_name` appears more than once in `policies`. Before, the provider sent all of them in one write

NOTES:

* gmail: `googleworkspace_gmail_send_as_alias` and `googleworkspace_user_delegate` now use `access_token` when it is set, as the documented precedence says. Before, the Gmail client ignored `access_token`, so a configuration with `access_token` and `credentials` (or application default credentials that can use domain-wide delegation) but no `service_account` worked for Gmail resources. Such a configuration now fails with "service_account is required to impersonate a user with the access_token authentication". Set `service_account` to a service account that has domain-wide delegation, and grant the principal of `access_token` the Service Account Token Creator role on it, or remove `access_token`. The new `googleworkspace_gmail_user_settings` resource has the same requirement.
* directory: the `googleworkspace_users` data source still shows `recovery_email` and `recovery_phone` in plain text. The provider SDK cannot mark one attribute inside the computed `users` list as sensitive, and marking the whole list sensitive would hide all user data
* directory: after the upgrade, the next refresh writes the new sensitive marks of `googleworkspace_user` to the state. A plan with `-refresh=false` instead shows a one-time update in place for each existing user. Applying that update makes no API call
* licensing: the new license resources need the `https://www.googleapis.com/auth/apps.licensing` scope. The provider does not request this scope by default, because a configuration that uses domain-wide delegation without this scope granted would fail to authenticate for all resources. The default scopes are unchanged. To manage license assignments, grant the scope to the service account and add it to the provider's `oauth_scopes`, together with the scopes that your other resources need.
* licensing: `googleworkspace_license_assignment` does not retry a `412 Precondition Failed` error from a license assignment or a SKU change, and fails at once. For a create, the two likely causes are that the user already has a license for the product (possibly with a different SKU; import the existing license assignment instead, with any SKU of the product in the import ID, because the refresh finds the current SKU) or that the SKU has no free seats. For a SKU change, the likely cause is that the new SKU has no free seats. Other `412` errors, for example from the Directory API, are still retried.
* licensing: the license resources also use the Directory API. Because `oauth_scopes` replaces the default scopes, set it to all the scopes that your configuration needs: `https://www.googleapis.com/auth/apps.licensing`; `https://www.googleapis.com/auth/admin.directory.user.readonly` or `admin.directory.user` (for `googleworkspace_license_assignment`, see below); and `https://www.googleapis.com/auth/admin.directory.customer.readonly` or `admin.directory.customer` (only if `customer_id` is `my_customer`). Each Directory API request of the license resources is limited to 15 seconds, so an admin role that cannot read users does not make a refresh or a create wait until its timeout.
* licensing: `googleworkspace_license_assignment` stores the user's immutable Directory user ID in the computed `directory_user_id` attribute, when the License Manager API or the Directory API finds `user_id` as the user's primary address (at create, at import, or at the first refresh of older state). If it is set, a refresh, a delete and a SKU change find the user by this ID and use the user's current primary address: they follow a rename (also when the old address is no longer an alias), never act on a different user that now has the address, and the refresh reads the license once, with no wait. If the user does not exist any more, a refresh removes the resource from state, a delete treats the license as gone, and a SKU change fails. If the lookup fails, a delete and a SKU change fail with the scope that they need and keep the state. Each refresh makes one Directory API request for each license assignment.
* licensing: without the Directory user scope, `directory_user_id` stays empty and `googleworkspace_license_assignment` uses only the License Manager API with the address in `user_id`. A renamed user is not followed: the refresh removes the license assignment from state, a destroy does not remove the license, and the next apply fails with a `412` error because the user already has the license; import the license assignment with the new address to recover.
* licensing: if the SKU of a `googleworkspace_license_assignment` is changed outside Terraform, a refresh finds the user's current SKU in a listing of the product, which is read once per provider configuration, and the plan changes `sku_id` back in place; the resource is removed from state only if the user has no license for the product. With `directory_user_id` set, a delete also finds and deletes the user's current SKU; if that listing fails, the delete treats the license as gone.
* licensing: a change to `user_id` of `googleworkspace_license_assignment` is in place, with no API write, only if the plan confirms that the new address is the current primary address of the user in `directory_user_id` and that it has the license. A different user or no license replaces the license assignment. An alias of the user, or any other error (for example a missing scope), fails the plan. A rename of the user's primary email address in the same apply cannot be confirmed at plan time and replaces the license assignment.
* licensing: a failed SKU change keeps the previous `sku_id` in state.
* licensing: if a refresh of `googleworkspace_license_assignment` finds that the user was renamed, it reports a warning with the user's current primary email address, to set in `user_id`.
* licensing: if the Directory API refuses a user lookup (a `403` error, or no answer within 15 seconds), the optional user lookups of `googleworkspace_license_assignment` (to store `directory_user_id`, and for a refresh without it) are skipped for the rest of the provider configuration. A delete or a SKU change with `directory_user_id` set still looks up the user and fails if the lookup fails.
* licensing: the License Manager API documents `customerId` as the unique customer ID. If the provider's `customer_id` is `my_customer`, the license resources get the unique customer ID from the Directory API (`customers.get`) once and cache it.
* gmail: `googleworkspace_gmail_user_settings` cannot delete Gmail settings. A destroy only removes the resource from the state and does not revert the IMAP, POP or automatic forwarding settings of the mailbox.
* gmail: automatic forwarding in `googleworkspace_gmail_user_settings` needs the `https://www.googleapis.com/auth/gmail.settings.sharing` scope (in the default scopes) with domain-wide delegation, and a forwarding address of the mailbox that already has the verification status `accepted`. Create the forwarding address outside Terraform; addresses in the same domain may not require verification. `enabled = false` turns off the automatic forwarding setting of one mailbox only: it is not the admin setting that controls whether users can forward mail, and it does not stop forwarding by Gmail filters.
* provider: three checks now fail earlier. `googleworkspace_user` checks the format of `primary_email`, and a missing password on create, at plan time instead of at apply time. Provider configuration fails when the universe domain is not `googleapis.com`, instead of when the provider calls an API
* docs: documented authentication without service account keys, the OAuth scopes of each resource and data source, network requirements for VPC Service Controls, government editions, and the data that debug logs contain

SECURITY:

* build: release binaries are built with Go 1.27.4. `go.mod` now sets `toolchain go1.27.4`, and the release workflow reads it. Earlier releases were built with Go 1.25.0 and carried known standard library vulnerabilities.
* deps: updated all Go module dependencies, including `golang.org/x/net`, `golang.org/x/crypto`, `google.golang.org/grpc` and `github.com/hashicorp/terraform-plugin-sdk/v2` (2.33.0 to 2.41.1). The dependency update does not change the provider schema. Building the provider now requires Go 1.26 or later, because the updated dependencies require it.
* ci: new `govulncheck` workflow on push, pull request, and weekly. Dependabot groups Go module updates.
* provider: debug logs (`TF_LOG=DEBUG` or higher) no longer show passwords or other secrets. Request and response bodies were scrubbed only for a top-level `accessToken`, so the `password` of `googleworkspace_user` and the SMTP password of `googleworkspace_gmail_send_as_alias` were written to the log in plain text. These JSON keys are now scrubbed at any depth, with a key match that is not case-sensitive: `password`, `newPassword`, `recoveryEmail`, `recoveryPhone`, `verificationCode`, `verificationCodes`, `passphrase`, `psk`, `preSharedKey` (Chrome network policy values, such as a Wi-Fi passphrase), `accessToken`, `access_token`, `refresh_token`, `id_token`, `client_secret`, `private_key` and `privateKey`. The values of the `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Goog-Api-Key` and `X-Goog-Iam-Authorization-Token` headers are scrubbed too. Bodies are read directly instead of from the wire dump, so chunked HTTP/1.1 responses and HTTP/2 responses are scrubbed. A body that is not exactly one JSON value (for example a multipart body) keeps its text, but the string value of each scrubbed key is replaced. A JSON body is logged indented with sorted keys, and numbers and HTML characters are kept as they are. A logging failure no longer makes the API call fail: a warning is logged and the body is left out of the debug log.

FEATURES:

* directory: added the write-only `password_wo` argument and the `password_wo_version` argument to `googleworkspace_user`. Terraform does not store `password_wo` in the plan or the state. Change `password_wo_version` to set a new password. `password_wo` conflicts with `password`, and `hash_function` applies to both. `password_wo_version` must be 1 or greater. To set a new hashed password, change `password_wo` and increment `password_wo_version`: a change to `hash_function` alone sends no password. A switch from `password` to `password_wo` sets the password through the API once, even if the value is the same. Google Workspace handles this as a password change, which can have side effects, for example on the sessions of the user. Write-only arguments require Terraform 1.11 or later
* **New Resource:** `googleworkspace_license_assignment` ([#476](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/476))
* **New Datasource:** `googleworkspace_license_assignments` ([#476](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/476))
* **New Resource:** `googleworkspace_gmail_user_settings`
* provider: added the `user_project` argument, which sends a quota project in the `x-goog-user-project` header of every API request, including the IAM Credentials `signJwt` and OAuth 2.0 token requests of domain-wide delegation. The `GOOGLE_CLOUD_QUOTA_PROJECT` environment variable, which the client library already read, is now documented; the library still applies it when `user_project` is empty. `user_project` applies to the Gmail resources (`googleworkspace_gmail_send_as_alias`, `googleworkspace_gmail_user_settings`, `googleworkspace_user_delegate`) together with the Gmail client fix in this release
* **New Datasource:** `googleworkspace_chrome_resolved_policies` reads the effective (resolved) Chrome policy values for an org unit, also the values that it inherits, with the org unit that sets each value. Use it in `check` blocks to verify a policy value, or that a policy is not set (a policy that is not set is absent, or has an empty `source_org_unit_id`: the default value for the customer). Values are in canonical form (each value has the type of its schema field). The API can omit a field at its default value, so read fields with `lookup(p.schema_values, "field", null)` or `try()`, and compare decoded values (`jsondecode`). If a value cannot be converted (for example, its schema cannot be read), the data source returns the raw API value of that policy, with one warning per schema. A wildcard filter that needs 15 or more schemas reads them with a few `policySchemas.list` calls instead of one request per schema. It needs the `chrome.management.policy` scope (in the default scopes) or the `chrome.management.policy.readonly` scope
* **New Resource:** `googleworkspace_chrome_group_policy` sets Chrome policies on a group (`groups/<id>` target). The API supports group targets only for app, printer, and print server policies (`chrome.users.apps.*`, `chrome.printers.*`, `chrome.printerservers.*`). It has the same validation, wait after a write, `applied_values`, and drift handling as `googleworkspace_chrome_policy`. A removed policy, and destroy, delete the policy from the group (`groups:batchDelete`). Plan and apply reject a policy schema that does not support groups. Refresh treats a value whose source is not the group (for example, an org unit) as not set. When the API reports that the group does not exist, refresh removes the resource with a warning, and destroy succeeds. A policy that is already not set on the group does not fail destroy or the reset before an update. The provider recognizes these errors (code 400 or 404) by their message, which must name the group ID or the policy schema; the messages are not verified against the live API. Import with `<group_id>/<schema names>` or `<group_id>/<key>=<value>+.../<schema names>`
* **New Resource:** `googleworkspace_chrome_policy_group_priority_ordering` sets which group has priority when more than one group sets a Chrome policy for an app. `policy_target_key` with the app in `additional_target_key_names` is required, and exactly one of `policy_schema` and `policy_namespace` must be set. `group_ids` must list every group that sets the policy for the app; the API rejects a partial list. The documentation describes how to add and remove a group. Refresh removes the ordering from state, with a warning, only when no group sets the policy; other API errors are returned. The API cannot delete an ordering, so destroy only removes it from state
* chromepolicy: `googleworkspace_chrome_policy` has a new optional block `additional_target_keys` (`target_key`, `target_value`), for policies that need more target keys, such as app policies (`chrome.users.apps.*`, key `app_id`). The keys apply to all policies of the resource, and a change forces a new resource. The order of the blocks does not matter. If no configured policy schema names additional target keys, the apply shows a warning. When the API resolves a policy for more than one target (for example, one for each app), the provider selects the policy whose target keys match. Without `additional_target_keys`, such a result is an error that tells to set them. A resolved policy for another target resource, or without additional target keys when the resource has them, is an error that names the returned target resource. When a policy schema names its target keys, plan and apply check that `additional_target_keys` has the same keys. State from an earlier version shows no change. The block format and the attribute names of the two new resources follow the macadmins/googleworkspace fork. Compatibility with that fork is not tested

IMPROVEMENTS:

* directory: `googleworkspace_user` shows a warning that suggests `password_wo` when the configuration sets `password`. The warning shows only with Terraform 1.11 or later, which supports write-only arguments. It also shows when `password` gets its value from a variable, even if that value can be null
* directory: `googleworkspace_user` now checks `primary_email` at plan time. A value without text before and after its last `@`, or a value with whitespace, fails validation. The check is loose and accepts valid addresses, such as plus addressing and subdomains. The API still does the full check. `on_delete_data_transfer.new_data_owner_email` is not checked, because it also accepts a unique user ID
* directory: `googleworkspace_user` now reports a missing password at plan time on create, not at apply time. A new user needs `password` or `password_wo`. An imported user can omit both
* directory: added `default_sender`, `favorite_replies_on_top`, `who_can_approve_members` and `who_can_ban_users` to `googleworkspace_group_settings` and its data source ([#489](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/489))
* provider: configuration now fails with a clear error when `GOOGLE_CLOUD_UNIVERSE_DOMAIN` or the `universe_domain` field of the credentials is not `googleapis.com`, because Google Workspace APIs and domain-wide delegation are only available in that universe. The error names the source of the value and how to correct it
* chromepolicy: `googleworkspace_chrome_policy` has a new computed attribute `applied_values`. When the API stores a policy value in another form than the configured value (for example, a normalized value), the provider records that form with a hash of the configured values. While the API value and the configured values stay the same, the plan shows no change for the policy, and an update of other policies does not wait for it. State from an earlier version has no recorded forms. If you upgrade from 0.12.0 or earlier with a value that the API stores in another form than your configuration (state has the API form), the plan shows a change, and each apply waits for the update timeout and shows the warning "Chrome Policy values did not change". This continues until the configuration uses the form that the plan shows
* chromepolicy: `googleworkspace_chrome_policy` treats a resolved policy without a source key as not set on the org unit, as the API documents (the value is the default value for the customer). Such a value is not accepted as the result of a write, and it is not restored after a failed write. Chrome Policy API requests, including schema requests, are now canceled when the operation is canceled
* chromepolicy: `googleworkspace_chrome_policy_schema` has new computed attributes: `category_title`, `supported_platforms`, `policy_api_lifecycle` (stage, description, end of support date, and replacement policies), `enum_fields` (the full value names, numbers, and descriptions of each enum field, also for enums in nested messages; a description comes from the known value that is the full name, or else the longest known value that is the last part of the name after `_`), and `field_known_values` (the known values of each field from the field descriptions)

BUG FIXES:

* gmail: `googleworkspace_gmail_send_as_alias` and `googleworkspace_user_delegate` now work with `access_token` and `service_account` authentication instead of failing with "Error 400: Precondition check failed" ([#39](https://github.com/SamuZad/terraform-provider-googleworkspace/issues/39))
* gmail: `googleworkspace_gmail_send_as_alias` now sends the mailbox owner's address (`primary_email`) as the Gmail user ID instead of `me`. With credentials that cannot impersonate a user, such as `gcloud` user application default credentials, the old read, update and delete calls acted on the send-as aliases of the caller's own mailbox (create already failed with such credentials); they now fail with a delegation error
* directory: `googleworkspace_user` now sends `hash_function` with each new password on update. Before, a changed hashed password was sent without its hash format
* directory: `relations.type` on `googleworkspace_user` now accepts `spouse`. The documentation and the API list `spouse`, but the validation rejected it
* directory: a `languages` entry with `custom_language` on `googleworkspace_user` no longer sends `language_code` and `preference`. Before, the provider always sent their defaults (`en` and `preferred`), which the API does not accept together with a custom language. When an entry sets `custom_language` in the configuration, the provider ignores the `language_code` and `preference` values of that entry, so they cause no diff. Entries that do not set `custom_language` in the configuration plan and send `language_code` and `preference` as before. This also applies when an entry changes from `custom_language` to `language_code`, when entries are reordered, and when a custom language was set outside Terraform
* gmail: `terraform import` of `googleworkspace_gmail_send_as_alias` now splits the ID at the first `:` only, so a send-as address that contains `:` (a quoted local part or an IPv6 address literal) can be imported. Before, such an ID failed with an unexpected format error. The ID format `primary-email:send-as-email` does not change, and existing IDs parse as before
* directory: `googleworkspace_group_members` now waits after create and update until the member listing shows the members it inserted or updated (with their roles) and no longer shows the members it deleted, instead of recording the first (possibly stale) listing in state. Members that the configuration does not declare do not block the wait. The time between checks grows from 3 to 30 seconds, and in a group with more than one page of members a few expected members are checked one by one. If the wait ends first, the resource gives a warning instead of an error. This builds on the approach in [#41](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/41) ([#40](https://github.com/SamuZad/terraform-provider-googleworkspace/issues/40))
* directory: `googleworkspace_group_members` and `googleworkspace_group_member` now compare member emails without regard to case, so a mixed-case email no longer causes a permanent diff, a delete and insert of the member, or a replacement
* directory: `googleworkspace_group_members` now finds the `delivery_settings` of a member without regard to email case
* directory: `googleworkspace_group_member` now keeps an inserted member in state and gives a warning when the consistency wait times out, instead of failing with "timed out while waiting for group_member to be inserted" ([hashicorp#475](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/475))
* directory: `googleworkspace_group_member` no longer sends an update request when no member attribute changed, and retries a transient 404 after an update
* directory: `googleworkspace_group_members` and `googleworkspace_group_member` now cancel API requests when the operation is canceled or times out. A canceled `googleworkspace_group_members` destroy stops with one error
* directory: `googleworkspace_group_members` and `googleworkspace_group_member` now adopt a member that already exists in the group ("Member already exists", 409) instead of failing, set its role and delivery settings to the configured values (also when that lowers the role), and give a warning that names the member and the changes. An apply after a consistency timeout or a failed create then converges. Destroying the resource removes an adopted member, and two resources that declare the same member both succeed. Another 409 is still an error. When an insert returns no member ID, the resources get the member by email
* directory: `googleworkspace_group_members` update now runs inserts and role updates before deletes, and does not delete a member that an insert adopted. Before, a configured alias (or the old address of a renamed user) could insert, adopt and then delete the same member. A delete that gets 404 counts as done, and a role update of a member removed outside Terraform inserts the member again. A failed update keeps the prior state
* directory: a failed create of `googleworkspace_group_members` or `googleworkspace_group_member` after the inserts (for example, an API error during the consistency wait) now saves no state, so the resource is not tainted and the next apply adopts the members instead of deleting them
* directory: `data.googleworkspace_group_member` now passes the request context to the API and returns the email as the API spells it
* directory: `googleworkspace_group_members` destroy now continues past a member that is already gone and tries to delete every member. Before, the first 404 stopped the loop and removed the resource from state, which left the other members in the group. Other errors are reported together after all members were tried
* directory: changing `group_id` of `googleworkspace_group_member` to another group now replaces the member. Before, the update addressed the member in the new group, which failed or removed the resource from state and left the member in the old group
* directory: `googleworkspace_group_member` and `googleworkspace_group_members` now replace the resource only when a changed `group_id` names another group. A value that differs only in case, or that names the same group by email, alias or ID (for example, after an import by group ID), updates state in place. Before, `googleworkspace_group_members` replaced the resource, which deleted and added again every member. If the group lookup fails at plan time with 403, the plan replaces the resource and logs a warning; another lookup error fails the plan
* chromepolicy: `googleworkspace_chrome_policy` now treats a policy that is no longer set as drift: refresh removes it from state with a warning, and the next plan sets it again. Before, the refresh failed with `unexpected number of resolved policies` ([#474](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/474))
* chromepolicy: `googleworkspace_chrome_policy` create and update now wait until the written values are visible, and can wait up to the new `create` and `update` timeouts (5 minutes by default). The write can be sent again: at half of that time if the values are not visible, and when a new value that differs from the configuration is seen, to confirm it (a failed resend is tried again and does not confirm a value). If they are still not visible, the apply shows a warning and saves the configured values. If the API stores a different form of a value (for example, a normalized value), that value is accepted only after the write is sent again and the value stays the same for two more checks. This applies to any new value that differs from the configuration, and to a value whose source became the org unit (when the policy was not set on the org unit before the change: not set, inherited from a parent, or the customer default). Then the apply shows the warning "Chrome Policy values differ from the configuration" and records the value. A value from another writer is overwritten only if the write that is sent again succeeds and the API applies it. A value that is equal to the value before the change, and whose source did not change, is not accepted in place of the configured value. If a policy still has the value from before the change at the timeout, after the write was sent again, the apply shows the warning "Chrome Policy values did not change", nothing is recorded, and the next plan shows a change. A policy that was reset (inherited) before the write and does not have the configured values at the timeout is an error, because the org unit can now use the inherited values. If the operation is canceled during the wait, the apply returns an error and records nothing ([#474](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/474))
* chromepolicy: `googleworkspace_chrome_policy` no longer rejects valid values for 32-bit integer fields, repeated fields, map fields, `null`, integers as decimal strings (also with an exponent, such as `"1e3"`), `float` and `double` values as strings (including `"NaN"`, `"Infinity"`, and `"-Infinity"`), enums as numbers, or schemas with fields of different types. One value (not a list) for a top-level repeated field, which earlier versions required, is still accepted and is sent as a list with one element, with no change in the plan
* chromepolicy: `googleworkspace_chrome_policy` update now resets (inherits) only the policies that are removed from the configuration or that have fields removed. Other policies are only written. If a write fails after a reset, the values from before the change are written again once, and the error tells if that succeeded. This restore also runs when the operation was canceled or its deadline passed (with its own limit of one minute), and the error says that the API can still apply the failed write later. This assumes that a failed write applies none of its changes. After a failed update, the state keeps the values from before the update
* chromepolicy: `googleworkspace_chrome_policy` no longer shows a change on each plan for numbers in repeated fields, map fields, or nested messages, or for a value in the configuration that is in a different form than the API value (for example an integer as a decimal string). After the upgrade, a configuration with such values can show one change. Apply it once
* chromepolicy: `googleworkspace_chrome_policy` now sends integers in the configuration to the API exactly, and keeps integers that the API returns exactly in state. Before, integers larger than 2^53 (for example `9007199254740993`) were rounded
* chromepolicy: `googleworkspace_chrome_policy` now accepts a map field as a list of objects with `key` and `value` fields, as well as a JSON object. Both forms of the same map are equal, so the plan shows no change between them
* chromepolicy: `googleworkspace_chrome_policy` now accepts the strings `"true"` and `"false"` for `TYPE_BOOL` fields, and treats them as `true` and `false`. Other strings, such as `"1"` or `"t"`, are not valid boolean values
* chromepolicy: `googleworkspace_chrome_policy` now checks a value against the field in the message of the policy (the message named by the last part of the schema name, for example `Foo` for `chrome.users.Foo`) first. Only if that message has no field with the name, it uses the first top-level field with the name. Before, a field with the same name in another message that came first in the schema was used

## 0.12.0 (August 29, 2026)

FEATURES:

* directory: added `aliases` to `googleworkspace_dynamic_group` and `data.googleworkspace_dynamic_group` ([#36](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/36))

IMPROVEMENTS:

* directory: improved handling of primary email changes combined with suspension or alias changes for `googleworkspace_user`, `googleworkspace_group` and `googleworkspace_group_settings` ([#35](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/35))
* directory: removed the default Google query suffix from all `googleworkspace_dynamic_group` queries ([#32](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/32))
* docs: documented `googleworkspace_user_delegate` and added an example ([#26](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/26))
* deps: updated `google.golang.org/grpc`, `golang.org/x/net` and `golang.org/x/crypto` ([#31](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/31), [#28](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/28), [#29](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/29))

BUG FIXES:

* directory: `googleworkspace_group_settings` is now removed from state on a 404 read (e.g. when its group is deleted outside Terraform) instead of failing the plan ([#30](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/30))
* directory: 404 responses during read are now handled by removing the resource from state ([#33](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/33))
* tests: fixed test compilation errors reported by `go vet` ([#26](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/26))

NOTES:

* The minimum Go version is now 1.25.0

## 0.11.5 (June 5, 2026)

IMPROVEMENTS:

* provider: improved retry handling ([#25](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/25))
* deps: updated `go.opentelemetry.io/otel` ([#24](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/24))

## 0.11.4 (April 15, 2026)

BUG FIXES:

* provider: HTTP 412 responses are now retried ([#23](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/23))

## 0.11.3 (April 10, 2026)

BUG FIXES:

* directory: user suspension is now handled separately from other `googleworkspace_user` updates ([#22](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/22))

## 0.11.2 (March 17, 2026)

BUG FIXES:

* directory: `googleworkspace_group_member` now fetches the member by email when the API returns an empty member ID on create

NOTES:

* deps: updated `github.com/cloudflare/circl` ([#20](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/20))

## 0.11.1 (November 24, 2025)

NOTES:

* The minimum Go version is now 1.24.0
* deps: updated `github.com/cloudflare/circl`, `golang.org/x/net`, `golang.org/x/oauth2` and `golang.org/x/crypto` ([#16](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/16), [#17](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/17), [#18](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/18), [#19](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/19))

## 0.11.0 (January 6, 2025)

FEATURES:

* **New Resource:** `googleworkspace_user_delegate` ([#12](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/12))

BUG FIXES:

* directory: the diff for `googleworkspace_group` aliases is now suppressed when only the order changes ([#11](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/11))

## 0.10.3 (December 6, 2024)

BUG FIXES:

* directory: suppress diffs on `googleworkspace_dynamic_group` labels

## 0.10.2 (December 6, 2024)

BUG FIXES:

* directory: improved handling of the default required label for `googleworkspace_dynamic_group`

NOTES:

* docs: regenerated

## 0.10.1 (October 14, 2024)

IMPROVEMENTS:

* directory: added `assignee_type` to `googleworkspace_role_assignment` so a role can be assigned to a group
* directory: added an option to add labels to `googleworkspace_dynamic_group`, for example to create security groups

## 0.10.0 (October 14, 2024)

FEATURES:

* **New Resource:** `googleworkspace_dynamic_group` ([#8](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/8))
* **New Datasource:** `googleworkspace_dynamic_group` ([#8](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/8))

## 0.9.5 (September 18, 2024)

BUG FIXES:

* directory: fixed alias suppression logic

NOTES:

* build: updated the GoReleaser configuration

## 0.9.4 (May 14, 2024)

BUG FIXES:

* directory: fixed the Drive and Docs data transfer on `googleworkspace_user` deletion, which never matched in 0.9.0 to 0.9.3

## 0.9.3 (May 11, 2024)

BUG FIXES:

* directory: fixed alias suppression

## 0.9.2 (May 11, 2024)

IMPROVEMENTS:

* directory: aliases are now sorted and diffs are suppressed on order changes ([#7](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/7))
* directory: added `ALL_OWNERS_CAN_LEAVE` to `googleworkspace_group_settings` ([#6](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/6))

## 0.9.1 (May 11, 2024)

IMPROVEMENTS:

* provider: removed the manual sleep, which is only needed under extreme concurrency

## 0.9.0 (May 11, 2024)

FEATURES:

* directory: added data transfer on deletion for `googleworkspace_user` ([#2](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/2))

IMPROVEMENTS:

* provider: improved retry handling ([#3](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/3))
* docs: made the advantages of domain wide delegation more explicit
* deps: updated Go dependencies

BUG FIXES:

* directory: added a default language to `googleworkspace_group` to stop permanent diffs ([#5](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/5))

NOTES:

* The minimum Go version is now 1.21 (toolchain 1.22.0)

## 0.8.2 (November 15, 2023)

IMPROVEMENTS:

* provider: lowered the consistency requirement used while waiting for changes to propagate

## 0.8.1 (August 24, 2023)

NOTES:

* deps: updated Go packages

## 0.8.0 (August 24, 2023)

NOTES:

* The minimum Go version is now 1.20
* deps: updated `google.golang.org/api` to v0.138.0
* build: adjusted the release build configuration

## 0.7.3 (August 24, 2023)

BUG FIXES:

* directory: a primary email change is now applied first so it does not conflict with other API operations, such as suspension or archival, in the same apply

## 0.7.2 (August 24, 2023)

BUG FIXES:

* directory: fixed an issue with user creation

## 0.7.1 (August 24, 2023)

NOTES:

* Added copyright and license headers, updated the MPL 2.0 license and README, and added CODEOWNERS ([#420](https://github.com/hashicorp/terraform-provider-googleworkspace/pull/420), [#407](https://github.com/hashicorp/terraform-provider-googleworkspace/pull/407), [#377](https://github.com/hashicorp/terraform-provider-googleworkspace/pull/377))
* build: changed the release workflow

BUG FIXES:

* directory: changing `primary_email` on `googleworkspace_user` now deletes the alias that Google keeps for the old primary email

## 0.7.0 (June 10, 2022)

FEATURES:

* **New Datasource:** `googleworkspace_groups` ([#313](https://github.com/hashicorp/terraform-provider-googleworkspace/pull/313))

IMPROVEMENTS:

* provider: added support for using the `GOOGLE_OAUTH_ACCESS_TOKEN` environment variable for authentication ([#319](https://github.com/hashicorp/terraform-provider-googleworkspace/pull/319))

BUG FIXES:

* directory: fixed bug where `custom_schemas` were not populating in `googleworkspace_users` [GH-279]
* directory: fixed bug where `recovery_phone` and `recovery_email` could not be removed from the `googleworkspace_user` resource [GH-261]
* directory: fixed bug where creation of `user`, `group`, `group_member`, and `group_settings`resources were affected by 404s from the API [GH-287]
* directory: fixed bug where creating a `googleworkspace_org_unit` would fail with `401` despite it being created [GH-327]

## 0.6.0 (December 20, 2021)

FEATURES:

* **New Datasource:** `googleworkspace_users` ([#240](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/240))

IMPROVEMENTS:

* directory: added `include_derived_membership` attribute to `data.googleworkspace_group_members` ([#234](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/234))
* directory: added `preference` attribute to `googleworkspace_user.languages` ([#238](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/238))

BUG FIXES:

* directory: fixed bug where data sources that allow exactly one field to be defined would drop the `id` field when the data source was recomputed. ([#218](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/218))
* directory: fixed bug where `googleworkspace_group_member` would crash instead of surfacing an error if needed ([#220](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/220))

## 0.5.2 (November 17, 2021)

BUG FIXES:

* directory: fixed bug where `googleworkspace_group_member` would error if a member already existed ([#211](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/211))
* directory: fixed bug where `googleworkspace_group_members` would only show the first 200 members ([#209](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/209))
* directory: fixed bug where `googleworkspace_user` showed a perma-diff on `languages.language_code` ([#208](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/208))
* directory: fixed bug where `googleworkspace_user` errored when adding `aliases` on initial create ([#208](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/208))
* groups settings: added validation options `ALL_OWNERS_CAN_CONTACT` validation to `who_can_contact_owner` and `ALL_OWNERS_CAN_VIEW` to `who_can_view_membership` and `who_can_view_group`. ([#207](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/207))

## 0.5.1 (October 27, 2021)

BUG FIXES:

* directory: fixed eventual consistency for `googleworkspace_group_member` ([#186](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/186))
* directory: fixed bug where `googleworkspace_group_members` would error if a member already existed ([#194](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/194))
* directory: fixed bug where `googleworkspace_group_members` would error if a `members` was empty ([#193](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/193))

## 0.5.0 (October 13, 2021)

FEATURES:

* **New Resource:**   `googleworkspace_group_members` ([#155](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/155))

* **New Datasource:** `googleworkspace_group_members` ([#155](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/155))

IMPROVEMENTS:

* provider: added ability to authenticate using user credentials ([#156](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/156))
* provider: added ability to authenticate using `access_token` rather than just credentials ([#165](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/165))
* provider: added retryTransport that will retry after common Google errors (like network errors, rate limiting errors, etc) ([#163](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/163))

BUG FIXES:

* directory: fixed bug where `googleworkspace_group_member` would not force new change on change of email ([#152](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/152))
* directory: fixed bug where `googleworkspace_user` would show permadiff on user alias emails from secondary domains ([#154](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/154))

## 0.4.1 (August 16, 2021)

BUG FIXES:

* provider: fixed validation of credentials ([#126](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/126))
* directory: fixed bug where `googleworkspace_user.password` was always required, which would break on import, now it is only required on create. ([#125](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/125))
* directory: fixed permadiff on`googleworkspace_user.custom_schemas` ([#129](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/129))
* directory: fixed bug where different fields on `googleworkspace_user` would error if the value was empty. ([#133](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/133))
* directory: fixed bugs on `googleworkspace_group`, `googleworkspace_group_member` and `googleworkspace_org_unit` where changes made out of band would not refresh appropriately. ([#136](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/136))
* directory: changed nested `type` fields on `googleworkspace_org_user` from optional to required. ([#139](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/139))
* gmail: fixed bugs on `googleworkspace_gmail_send_as_alias` where changes made out of band would not refresh appropriately. ([#136](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/136))

## 0.4.0 (July 27, 2021)

FEATURES:

* **New Resource:** `googleworkspace_gmail_send_as_alias` ([#122](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/122))

IMPROVEMENTS:

* provider: allow impersonation to be unset when service account has sufficient role assignments. ([#115](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/115))

BUG FIXES:

* groups settings: fixed bug where consistency check was prone to failure/timeout. ([#119](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/119))
* directory: fixed bug where `googleworkspace_schema.fields.indexed` would break if it was nil. ([#108](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/108))
* chrome: fixed validation bug on message type schema values, ([#116](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/116))

## 0.3.0 (July 07, 2021)

FEATURES:

* **New Resource:** `googleworkspace_chrome_policy` ([#97](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/97))
* **New Resource:** `googleworkspace_domain_alias` ([#92](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/92))

* **New Datasource:**   `googleworkspace_chrome_policy_schema` ([#97](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/97))
* **New Datasource:**   `googleworkspace_domain_alias` ([#92](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/92))

IMPROVEMENTS:

* provider: added support for using the `GOOGLE_CREDENTIALS` environment variable for authentication ([#87](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/87))

BUG FIXES:

* all: added logging of the http requests/responses ([#93](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/93))

## 0.2.0 (June 21, 2021)

FEATURES:

* **New Resource:** `googleworkspace_org_unit` ([#63](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/63))
* **New Resource:** `googleworkspace_role` ([#66](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/66))
* **New Resource:** `googleworkspace_role_assignment` ([#66](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/66))

* **New Datasource:**   `googleworkspace_org_unit` ([#63](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/63))
* **New Datasource:**   `googleworkspace_role` ([#66](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/66))
* **New Datasource:**   `googleworkspace_privileges` ([#82](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/82))

## 0.1.0 (June 03, 2021)

FEATURES:

* **New Resource:** `googleworkspace_domain` ([#12](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/12))
* **New Resource:** `googleworkspace_group` ([#18](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/18))
* **New Resource:** `googleworkspace_group_member` ([#31](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/31))
* **New Resource:** `googleworkspace_group_settings` ([#29](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/29))
* **New Resource:** `googleworkspace_schema` ([#20](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/20))
* **New Resource:** `googleworkspace_user` ([#15](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/15))

* **New Datasource:**   `googleworkspace_domain` ([#12](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/12))
* **New Datasource:**   `googleworkspace_group` ([#18](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/18))
* **New Datasource:**   `googleworkspace_group_member` ([#31](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/31))
* **New Datasource:**   `googleworkspace_group_settings` ([#32](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/32))
* **New Datasource:**   `googleworkspace_schema` ([#20](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/20))
* **New Datasource:**   `googleworkspace_user` ([#15](https://github.com/hashicorp/terraform-provider-googleworkspace/issues/15))
