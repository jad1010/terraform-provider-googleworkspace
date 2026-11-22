---
layout: ""
page_title: "Google Workspace Provider"
subcategory: ""
description: |-
The Google Workspace provider provides resources to interact with Google Workspace.
---

# Google Workspace Provider

  The Google Workspace provider provides resources to interact with Google Workspace.

## Example Usage

```terraform
# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# Auth method: Domain-wide delegation and user impersonation
provider "googleworkspace" {
  credentials             = "/Users/mscott/my-project-c633d7053aab.json"
  customer_id             = "A01b123xz"
  impersonated_user_email = "impersonated@example.com"
  oauth_scopes = [
    "https://www.googleapis.com/auth/admin.directory.user",
    "https://www.googleapis.com/auth/admin.directory.userschema",
    # include scopes as needed
  ]
}
```

```terraform
# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# Auth method: Admin roles applied directly to a service account
provider "googleworkspace" {
  credentials = "/Users/mscott/my-project-c633d7053aab.json"
  customer_id = "A01b123xz"
  oauth_scopes = [
    "https://www.googleapis.com/auth/admin.directory.user",
    "https://www.googleapis.com/auth/admin.directory.userschema",
    # include scopes as needed
  ]
}
```

## Authorization
This provider uses [Admin SDK](https://developers.google.com/admin-sdk) API methods to manage resources on a Workspace customer domain. There are multiple ways to set up proper authorization for a service account:

* Enable [domain-wide delegation](#using-domain-wide-delegation) to impersonate a user that has super administrator privileges. You cannot directly grant super administrator privileges to service accounts. Therefore **domain-wide delegation is necessary for managing resources that require super administrator privileges, such as other super administrator users**.

* Assign [specific administrator roles](https://support.google.com/a/answer/9807615?hl=en&ref_topic=9832445) directly to the service account.

## Authentication

### Using Domain-Wide Delegation

#### Creating a Service Account and Credentials

Terraform uses a GCP service account to manage resources created by the provider. To create the service account and generate a service account key:

1. Follow the instructions in the [create service account and credentials documentation](https://developers.google.com/admin-sdk/directory/v1/guides/delegation#create_the_service_account_and_credentials).
2. Save the json file containing your service account key credentials locally and set the `GOOGLEWORKSPACE_CREDENTIALS` environment variable to the path of that file. Terraform will now use that key for authentication.
   * Other environment variable options include: `GOOGLEWORKSPACE_CLOUD_KEYFILE_JSON` or `GOOGLE_CREDENTIALS` (>= 0.2.0)

#### Configuring the Service Account's granted scopes in Google Workspace

To access user data on a Google Workspace domain, the service account that you created needs to be granted access
by a super administrator for the domain. Follow the instructions in the
[delegate domain-wide authority documentation](https://developers.google.com/admin-sdk/directory/v1/guides/delegation#delegate_domain-wide_authority_to_your_service_account).

* Note: The Oauth scopes granted to your service account must match, or be a superset, of the `oauth_scopes` granted to
the `googleworkspace` provider.

More information about scopes is below.

#### Configuring scopes requested by the provider

The provider can be configured with an `oauth_scopes` field containing a list of requested scopes. If `oauth_scopes` is _not_ set in the provider configuration, the provider will fall back to a default list of scopes, which are all the scopes needed to manage resources that can be provisioned by the provider, except the licensing scope (see [Scopes for license assignments](#scopes-for-license-assignments)) ([see default scopes for v0.6.0 here](https://github.com/hashicorp/terraform-provider-googleworkspace/blob/v0.6.0/internal/provider/provider.go#L17-L30)). If default scopes are used, the service account needs to be granted all of those scopes in Google Workspace.

The scopes declared in the provider's configuration need to match, or be a subset of, the scopes granted to the service account. If a provider is configured with scopes the service account isn't granted to use, the provider will receive a `401 Unauthorized` response when it requests an access token.

->It's recommended to include `oath_scopes` in your provider configuration to make the requested scopes explicit and easier to debug issues.

#### Scopes for license assignments

The `googleworkspace_license_assignment` resource and the `googleworkspace_license_assignments` data source use the
[Enterprise License Manager API](https://developers.google.com/workspace/admin/licensing/v1/how-tos/using),
which requires the `https://www.googleapis.com/auth/apps.licensing` scope. This scope is **not** in the default list of
scopes. If the provider requested it by default, each configuration that uses domain-wide delegation without this
scope granted to the service account would fail to get an access token, also for all other resources.

The license resources also read from the Directory API. All the scopes that they use are:

| Scope | Used by |
|-------|---------|
| `https://www.googleapis.com/auth/apps.licensing` | Always. |
| `https://www.googleapis.com/auth/admin.directory.user.readonly` (or `admin.directory.user`, a default scope) | `googleworkspace_license_assignment`: to store `directory_user_id`, to follow a renamed user, to check a change to `user_id` at plan time, and to find the user by `directory_user_id` before a refresh, a delete or a SKU change. With `directory_user_id` set, each refresh makes one Directory API request for each license assignment. Without this scope, or if the admin role cannot read users, `directory_user_id` stays empty and the resource uses only the License Manager API: a renamed user is not followed (the refresh removes the license assignment from state, a destroy does not remove the license, and the next apply fails with a `412` error until you import the license assignment with the new address), and a change to `user_id` replaces the license assignment. See the resource documentation. |
| `https://www.googleapis.com/auth/admin.directory.customer.readonly` (or `admin.directory.customer`, a default scope) | Both, only if `customer_id` is `my_customer`: the License Manager API documents `customerId` as the unique customer ID, so the provider gets it from the Directory API. |

To manage license assignments:

1. Enable the Enterprise License Manager API (`licensing.googleapis.com`) in the Google Cloud project of the service account.
2. If you use domain-wide delegation, grant the scopes to the service account in the Admin console.
3. Set `oauth_scopes` in the provider configuration. `oauth_scopes` replaces the default list, so include all the
   scopes that your other resources need. For example, the default scopes (which include `admin.directory.user`
   and `admin.directory.customer`) plus the licensing scope:

```terraform
provider "googleworkspace" {
  customer_id             = "A01b123xz"
  impersonated_user_email = "impersonated@example.com"
  oauth_scopes = [
    "https://www.googleapis.com/auth/gmail.settings.basic",
    "https://www.googleapis.com/auth/gmail.settings.sharing",
    "https://www.googleapis.com/auth/chrome.management.policy",
    "https://www.googleapis.com/auth/cloud-platform",
    "https://www.googleapis.com/auth/admin.directory.customer",
    "https://www.googleapis.com/auth/admin.directory.domain",
    "https://www.googleapis.com/auth/admin.directory.group",
    "https://www.googleapis.com/auth/admin.directory.orgunit",
    "https://www.googleapis.com/auth/admin.directory.rolemanagement",
    "https://www.googleapis.com/auth/admin.directory.userschema",
    "https://www.googleapis.com/auth/admin.directory.user",
    "https://www.googleapis.com/auth/apps.groups.settings",
    "https://www.googleapis.com/auth/apps.licensing",
  ]
}
```

If you authenticate with `access_token` without `impersonated_user_email`, the provider ignores `oauth_scopes`. The token must then already include the scopes above.


#### Impersonating a Google Workspace User

Only users with access to the Admin APIs can access the Admin SDK Directory API, therefore your service account needs to impersonate one of those users to access the Admin SDK Directory API. This user's email
must be set in the environment variable `GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL` or in the `impersonated_user_email` attribute in the provider. Additionally, the user must have logged in at least once and accepted the Google Workspace Terms of Service.

### Using Specific Administrator Roles

You do not need to set up domain-wide delegation if you are granting more specific administrator roles to the service account. If the Terraform pipeline execution environment provides an appropriate token as Application Default Credentials (ADC), you can use the provider without any further setup.

To do this it's recommended that you create a custom admin role(s) with the Admin API privileges you need, as [pre-built administrator roles](https://support.google.com/a/answer/2405986) might not cover your use case.

~> Some resources controlled via this provider can only be managed by a Super Admin, so user impersonation is necessary in those cases. For instance, [Admin Roles can only managed via the console or API by a Super Admin](https://support.google.com/a/answer/2406043?hl=en). Also, access to some API endpoints may not be possible to grant as privileges in custom roles. 

When using gcloud locally, you can provide the required scopes for ADC login by adding the `--scopes` parameter to [`gcloud auth application-default login`](https://cloud.google.com/sdk/gcloud/reference/auth/application-default/login). For example, you can [provide additional scopes](https://cloud.google.com/sdk/gcloud/reference/beta/compute/instances/set-scopes) on Compute Engine. You can do this to configure access for both service accounts and end users.

```terraform
provider "googleworkspace" {
  customer_id = "A01b123xz"
}
```

The approach outlined above does not work on Cloud Build because it does not (yet) support specifying additional scopes for service account tokens accessible during builds. Other pipeline setups use the `google_service_account_access_token` to impersonate a service account. This allows them to use a single identity regardless of who is initiating the execution. For these cases, set the `access_token` parameter to the appropriate credentials.

```terraform
provider "googleworkspace" {
  customer_id  = "A01b123xz"
  access_token = data.google_service_account_access_token.default.access_token
}
```

Gmail resources (`googleworkspace_gmail_send_as_alias` and `googleworkspace_user_delegate`) always impersonate the mailbox owner. With `access_token`, they need `service_account`: the principal of the access token must have the `Service Account Token Creator` role on that service account, and the service account must have [domain-wide delegation](#using-domain-wide-delegation) for the Gmail scopes.

```terraform
provider "googleworkspace" {
  customer_id     = "A01b123xz"
  access_token    = data.google_service_account_access_token.default.access_token
  service_account = "terraform@my-project.iam.gserviceaccount.com"
}
```

You can also provide an exported service account key in the `credentials` parameter without specifying an `impersonated_user_email`.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `access_token` (String) A temporary [OAuth 2.0 access token] obtained from the Google Authorization server, i.e. the `Authorization: Bearer` token used to authenticate HTTP requests to Google Admin SDK APIs. This is an alternative to `credentials`, and ignores the `oauth_scopes` field. If both are specified, `access_token` will be used over the `credentials` field.
- `credentials` (String) Either the path to or the contents of a service account key file in JSON format you can manage key files using the Cloud Console).  If not provided, the application default credentials will be used.
- `customer_id` (String) The customer id provided with your Google Workspace subscription. It is found in the admin console under Account Settings.
- `impersonated_user_email` (String) The impersonated user's email with access to the Admin APIs can access the Admin SDK Directory API. `impersonated_user_email` is required for all services except group and user management.
- `oauth_scopes` (List of String) The list of the scopes required for your application (for a list of possible scopes, see [Authorize requests](https://developers.google.com/admin-sdk/directory/v1/guides/authorizing))
- `service_account` (String) The service account used to create the provided `access_token` if authenticating using the `access_token` method and needing to impersonate a user. This service account will require the GCP role `Service Account Token Creator` if needing to impersonate a user. Gmail resources (`googleworkspace_gmail_send_as_alias`, `googleworkspace_user_delegate`) always impersonate the mailbox owner, so with `access_token` they need `service_account`, the `Service Account Token Creator` role for the `access_token` principal on it, and domain-wide delegation for it.
