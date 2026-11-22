## 0.13.0 (Unreleased)

BREAKING CHANGES:

* directory: `recovery_email` and `recovery_phone` are now sensitive in the `googleworkspace_user` resource and in the `googleworkspace_user` data source. Terraform hides their values in plan and apply output. A root module output that refers to these values now needs `sensitive = true`. To use a value where Terraform does not accept sensitive values, such as `for_each` or `count`, wrap it in `nonsensitive()`

NOTES:

* gmail: `googleworkspace_gmail_send_as_alias` and `googleworkspace_user_delegate` now use `access_token` when it is set, as the documented precedence says. Before, the Gmail client ignored `access_token`, so a configuration with `access_token` and `credentials` (or application default credentials that can use domain-wide delegation) but no `service_account` worked for Gmail resources. Such a configuration now fails with "service_account is required to impersonate a user with the access_token authentication". Set `service_account` to a service account that has domain-wide delegation, and grant the principal of `access_token` the Service Account Token Creator role on it, or remove `access_token`.
* directory: the `googleworkspace_users` data source still shows `recovery_email` and `recovery_phone` in plain text. The provider SDK cannot mark one attribute inside the computed `users` list as sensitive, and marking the whole list sensitive would hide all user data
* directory: after the upgrade, the next refresh writes the new sensitive marks of `googleworkspace_user` to the state. A plan with `-refresh=false` instead shows a one-time update in place for each existing user. Applying that update makes no API call

SECURITY:

* build: release binaries are built with Go 1.27.4. `go.mod` now sets `toolchain go1.27.4`, and the release workflow reads it. Earlier releases were built with Go 1.25.0 and carried known standard library vulnerabilities.
* deps: updated all Go module dependencies, including `golang.org/x/net`, `golang.org/x/crypto`, `google.golang.org/grpc` and `github.com/hashicorp/terraform-plugin-sdk/v2` (2.33.0 to 2.41.1). The dependency update does not change the provider schema. Building the provider now requires Go 1.26 or later, because the updated dependencies require it.
* ci: new `govulncheck` workflow on push, pull request, and weekly. Dependabot groups Go module updates.
* provider: debug logs (`TF_LOG=DEBUG` or higher) no longer show passwords or other secrets. Request and response bodies were scrubbed only for a top-level `accessToken`, so the `password` of `googleworkspace_user` and the SMTP password of `googleworkspace_gmail_send_as_alias` were written to the log in plain text. These JSON keys are now scrubbed at any depth, with a key match that is not case-sensitive: `password`, `newPassword`, `recoveryEmail`, `recoveryPhone`, `verificationCode`, `verificationCodes`, `accessToken`, `access_token`, `refresh_token`, `id_token`, `client_secret`, `private_key` and `privateKey`. The values of the `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Goog-Api-Key` and `X-Goog-Iam-Authorization-Token` headers are scrubbed too. Bodies are read directly instead of from the wire dump, so chunked HTTP/1.1 responses and HTTP/2 responses are scrubbed. A body that is not exactly one JSON value (for example a multipart body) keeps its text, but the string value of each scrubbed key is replaced. A JSON body is logged indented with sorted keys, and numbers and HTML characters are kept as they are. A logging failure no longer makes the API call fail: a warning is logged and the body is left out of the debug log.

FEATURES:

* directory: added the write-only `password_wo` argument and the `password_wo_version` argument to `googleworkspace_user`. Terraform does not store `password_wo` in the plan or the state. Change `password_wo_version` to set a new password. `password_wo` conflicts with `password`, and `hash_function` applies to both. `password_wo_version` must be 1 or greater. To set a new hashed password, change `password_wo` and increment `password_wo_version`: a change to `hash_function` alone sends no password. A switch from `password` to `password_wo` sets the password through the API once, even if the value is the same. Google Workspace handles this as a password change, which can have side effects, for example on the sessions of the user. Write-only arguments require Terraform 1.11 or later

IMPROVEMENTS:

* directory: `googleworkspace_user` now reports a missing password at plan time on create, not at apply time. A new user needs `password` or `password_wo`. An imported user can omit both
* directory: `googleworkspace_user` shows a warning that suggests `password_wo` when the configuration sets `password`. The warning shows only with Terraform 1.11 or later, which supports write-only arguments. It also shows when `password` gets its value from a variable, even if that value can be null

BUG FIXES:

* gmail: `googleworkspace_gmail_send_as_alias` and `googleworkspace_user_delegate` now work with `access_token` and `service_account` authentication instead of failing with "Error 400: Precondition check failed" ([#39](https://github.com/SamuZad/terraform-provider-googleworkspace/issues/39))
* gmail: `googleworkspace_gmail_send_as_alias` now sends the mailbox owner's address (`primary_email`) as the Gmail user ID instead of `me`. With credentials that cannot impersonate a user, such as `gcloud` user application default credentials, the old read, update and delete calls acted on the send-as aliases of the caller's own mailbox (create already failed with such credentials); they now fail with a delegation error
* directory: `googleworkspace_user` now sends `hash_function` with each new password on update. Before, a changed hashed password was sent without its hash format

## 0.8.0 (Unreleased)

BUG FIXES:

* directory: `googleworkspace_group_settings` is now removed from state on a 404 read (e.g. when its group is deleted outside Terraform) instead of failing the plan ([#30](https://github.com/SamuZad/terraform-provider-googleworkspace/pull/30))

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
