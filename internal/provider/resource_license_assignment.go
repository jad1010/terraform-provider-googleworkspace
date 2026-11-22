// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	directory "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/licensing/v1"
)

// licensingClientScope is the OAuth scope of the Enterprise License Manager API.
// It is not in DefaultClientScopes: see the provider documentation.
const licensingClientScope = "https://www.googleapis.com/auth/apps.licensing"

// The license resources also read users and the customer from the Directory
// API. The read-only scopes are enough; the full scopes also work.
const (
	directoryUserReadonlyScope     = "https://www.googleapis.com/auth/admin.directory.user.readonly"
	directoryCustomerReadonlyScope = "https://www.googleapis.com/auth/admin.directory.customer.readonly"
)

// licenseAssignmentIdSeparator separates the parts of a license assignment ID.
// Product IDs, SKU IDs and Google Workspace user emails cannot contain it.
const licenseAssignmentIdSeparator = "/"

// licenseDirectoryLookupTimeout limits each Directory API request of the
// license resources. The retry transport retries a 403 "Not Authorized to
// access this resource/api" until the context ends (see
// isCommonRetryableErrorCode). A principal that has the scope but whose admin
// role cannot read users gets that error every time, so without this limit a
// refresh or a create would wait until its timeout.
var licenseDirectoryLookupTimeout = 15 * time.Second

// licenseAssignmentScopesDescription is the scope part of the resource description.
const licenseAssignmentScopesDescription = "This resource uses these OAuth scopes. The provider's default " +
	"scopes do not include `" + licensingClientScope + "`, and `oauth_scopes` replaces the default list, so set " +
	"`oauth_scopes` to all the scopes that your configuration needs:\n\n" +
	"- `" + licensingClientScope + "` (always).\n" +
	"- `" + directoryUserReadonlyScope + "` or `https://www.googleapis.com/auth/admin.directory.user` " +
	"(to store `directory_user_id`, to follow a renamed user, to check a change to `user_id`, and to find the " +
	"user of the license assignment by `directory_user_id` before a refresh, a delete or a SKU change). With " +
	"`directory_user_id` set, each refresh makes one Directory API request for each license assignment. " +
	"Without this scope, or if the admin role of the principal cannot read users, `directory_user_id` stays " +
	"empty and the resource uses only the License Manager API with the address in `user_id`: a refresh that " +
	"does not find the license lists the product by `user_id` to find another SKU of the user, and removes " +
	"the resource from state if it finds none; a delete that does not find the license treats it as gone; a " +
	"change to `user_id` replaces the license assignment. A renamed user is not followed: the refresh removes " +
	"the license assignment from state, a destroy does not remove the license, and the next apply tries to " +
	"assign the license again and fails with a `412` error because the user already has it. Import the " +
	"license assignment with the user's new address to recover. If `directory_user_id` is set and the scope " +
	"is later removed, a refresh that does not find the license, a delete and a SKU change fail with an error " +
	"that names the scope.\n" +
	"- `" + directoryCustomerReadonlyScope + "` or `https://www.googleapis.com/auth/admin.directory.customer` " +
	"(only if `customer_id` is `my_customer`).\n\n"

func resourceLicenseAssignment() *schema.Resource {
	return &schema.Resource{
		// This description is used by the documentation generator and the language server.
		Description: "License Assignment resource in the Terraform Googleworkspace provider. It assigns a " +
			"Google Workspace product SKU license to a user.\n\n" +
			licenseAssignmentScopesDescription +
			"A create or a SKU change that gets a `412 Precondition Failed` error fails at once and is not retried. " +
			"For a create, the two likely causes are:\n\n" +
			"- The user already has a license for this product, possibly with a different SKU. A user can have only " +
			"one SKU of a product, and Google Workspace can assign a license automatically. Import the existing " +
			"license assignment with the ID `<product_id>/<sku_id>/<user_id>`. Any SKU of the product works in the " +
			"import ID, because the refresh finds the user's current SKU. Then set `sku_id` to the SKU that you want.\n" +
			"- The SKU has no free seats. Make licenses available, then apply again.\n\n" +
			"For a SKU change, the likely cause is that the new SKU has no free seats.\n\n" +
			"Other behavior to know:\n\n" +
			"- The resource stores the user's immutable Directory user ID in `directory_user_id`. If it is set, a " +
			"refresh, a delete and a SKU change find the user by this ID and use the user's current primary email " +
			"address, so they follow a rename (also when the old address is no longer an alias) and never act on " +
			"a different user that now has the address in `user_id`. If the user does not exist any more, a refresh " +
			"removes the resource from state, a delete treats the license as gone, and a SKU change fails. " +
			"`user_id` keeps the configured value. `directory_user_id` is set when the License Manager API or the " +
			"Directory API finds `user_id` as the user's primary address: at create, at import, or at the first " +
			"refresh of state from before this attribute. Until then, an address that the Directory API finds " +
			"only as an alias is treated as not found.\n" +
			"- If the SKU is changed outside Terraform, a refresh finds the user's current SKU of the product " +
			"and the plan changes `sku_id` back in place. The resource is removed from state only if the user " +
			"has no license for the product. To find the SKU, the refresh lists the license assignments of the " +
			"product once for each provider configuration, which can take time for a large customer. With " +
			"`directory_user_id` set, a delete that does not find the license with the stored SKU also lists the " +
			"product, and deletes the user's license of the product with its current SKU. If that listing fails, " +
			"or without `directory_user_id`, such a delete treats the license as gone.\n" +
			"- If a refresh finds that the user was renamed, it reports a warning with the user's current primary " +
			"email address. Set `user_id` to that address.\n" +
			"- If the Directory API refuses a user lookup (a `403` error, or no answer within 15 seconds), the " +
			"optional user lookups (to store `directory_user_id`, and for a refresh without it) are skipped for " +
			"the rest of the provider configuration. A delete or a SKU change with `directory_user_id` set still " +
			"looks up the user, and fails if the lookup fails.\n" +
			"- A change to `user_id` is done in place, with no API write, only if the plan confirms that the " +
			"new value is the current primary email address of the user in `directory_user_id`, and the License " +
			"Manager API returns the assignment for the new address. If the new address belongs to a different " +
			"user or has no license, the change replaces the license assignment. If the new address is an alias " +
			"of the user, the plan fails, because `user_id` must be the primary address. If the check fails for " +
			"another reason, for example a missing scope, the plan fails. A rename of " +
			"`googleworkspace_user.primary_email` in the same apply as the license assignment cannot be confirmed " +
			"at plan time, because the user is not renamed yet, so it replaces the license assignment. To keep " +
			"the license, apply the rename first with the old `user_id`, then change `user_id`.\n" +
			"- The create retries a `404` error or a `400 User email not valid` error until the create timeout, " +
			"because a new user is not immediately visible to the License Manager API. A typo in `user_id` " +
			"therefore fails only when the create timeout ends.",

		CustomizeDiff: resourceLicenseAssignmentCustomizeDiff,

		CreateContext: resourceLicenseAssignmentCreate,
		ReadContext:   resourceLicenseAssignmentRead,
		UpdateContext: resourceLicenseAssignmentUpdate,
		DeleteContext: resourceLicenseAssignmentDelete,

		Importer: &schema.ResourceImporter{
			StateContext: resourceLicenseAssignmentImport,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(5 * time.Minute),
			Delete: schema.DefaultTimeout(5 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"id": {
				Description: "ID of the license assignment, in the format `<product_id>/<sku_id>/<user_id>`.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"product_id": {
				Description: "The product's unique identifier, for example `Google-Apps`. " +
					"See [Products and SKUs](https://developers.google.com/workspace/admin/licensing/v1/how-tos/products).",
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},
			"sku_id": {
				Description: "The product SKU's unique identifier, for example `1010020027`. " +
					"See [Products and SKUs](https://developers.google.com/workspace/admin/licensing/v1/how-tos/products). " +
					"A change reassigns the user to the new SKU of the same product in place.",
				Type:     schema.TypeString,
				Required: true,
			},
			"user_id": {
				Description: "The user's primary email address. A change replaces the license assignment, " +
					"unless the plan confirms that the new address is the same user with the same license.",
				Type:     schema.TypeString,
				Required: true,
				DiffSuppressFunc: func(k, old, new string, d *schema.ResourceData) bool {
					return strings.EqualFold(old, new)
				},
			},
			"directory_user_id": {
				Description: "The user's immutable unique ID in the Directory API. The provider uses it to find " +
					"the user of the license assignment after a rename, and to never act on a different user.",
				Type:     schema.TypeString,
				Computed: true,
			},
			"etag": {
				Description: "ETag of the resource.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"product_name": {
				Description: "Display name of the product.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"sku_name": {
				Description: "Display name of the product SKU.",
				Type:        schema.TypeString,
				Computed:    true,
			},
			"self_link": {
				Description: "Link to this license assignment in the API.",
				Type:        schema.TypeString,
				Computed:    true,
			},
		},
	}
}

func licenseAssignmentId(productId, skuId, userId string) string {
	return strings.Join([]string{productId, skuId, userId}, licenseAssignmentIdSeparator)
}

func parseLicenseAssignmentId(id string) (productId, skuId, userId string, err error) {
	parts := strings.Split(id, licenseAssignmentIdSeparator)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", fmt.Errorf("License Assignment Id (%s) is not of the correct format (<product_id>/<sku_id>/<user_id>)", id)
	}

	return parts[0], parts[1], parts[2], nil
}

// isLicenseUserNotReady reports whether a License Manager API error means
// that the API does not find the user or the license: a 404, or a 400 "User
// email not valid". The API returns these for a user that it does not see yet
// (for example immediately after the user was created), for an address that
// is not the user's current primary email, and for a user without the license.
func isLicenseUserNotReady(err error) bool {
	var gerr *googleapi.Error
	if !errors.As(err, &gerr) {
		return false
	}

	return gerr.Code == http.StatusNotFound ||
		(gerr.Code == http.StatusBadRequest && strings.Contains(gerr.Error(), "User email not valid"))
}

// licenseInsertError adds an explanation to a 412 Precondition Failed error
// from a license insert. The insert is not retried on a 412, because the
// causes are not transient.
func licenseInsertError(err error, productId, skuId, userId string) error {
	if !isApiErrorWithCode(err, http.StatusPreconditionFailed) {
		return err
	}

	return fmt.Errorf("Error assigning license (product %s, sku %s) to user %s: %w. "+
		"Two causes are likely. "+
		"(1) The user already has a license for this product, possibly with a different SKU, for example a license "+
		"that Google Workspace assigned automatically. A user can have only one SKU of a product. Import the existing "+
		"license assignment instead, with the ID %s: the refresh after the import finds the user's current SKU of the "+
		"product, and the plan then changes it to the configured sku_id. "+
		"(2) The SKU has no free seats. Make licenses available, then apply again",
		productId, skuId, userId, err, licenseAssignmentId(productId, skuId, userId))
}

// licensePatchError adds an explanation to a 412 Precondition Failed error
// from a license reassignment. The patch is not retried on a 412, because the
// causes are not transient.
func licensePatchError(err error, productId, oldSkuId, newSkuId, userId string) error {
	if !isApiErrorWithCode(err, http.StatusPreconditionFailed) {
		return err
	}

	return fmt.Errorf("Error reassigning license (product %s) of user %s from sku %s to sku %s: %w. "+
		"The new SKU likely has no free seats. Make licenses available, then apply again",
		productId, userId, oldSkuId, newSkuId, err)
}

// directoryScopeError adds the Directory API scopes to an error.
func directoryScopeError(action string, err error, scope string) error {
	return fmt.Errorf("Error %s with the Directory API: %w. This needs the %s scope, or the same scope "+
		"without .readonly, in the provider's oauth_scopes, and an admin role that can read it", action, err, scope)
}

// errLicenseUserGone means that the user in directory_user_id does not exist
// any more.
var errLicenseUserGone = errors.New("the user of this license assignment does not exist any more")

// getLicenseDirectoryUser gets a user from the Directory API by a primary
// email address, an alias or a unique user ID, within
// licenseDirectoryLookupTimeout. It returns nil if the user is not found.
func getLicenseDirectoryUser(ctx context.Context, client *apiClient, userKey string) (*directory.User, error) {
	directoryService, diags := client.NewDirectoryService()
	if diags.HasError() {
		return nil, fmt.Errorf("%s", diags[0].Summary)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, licenseDirectoryLookupTimeout)
	defer cancel()

	user, err := directoryService.Users.Get(userKey).Context(lookupCtx).Do()
	if isApiErrorWithCode(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		var gerr *googleapi.Error
		if (errors.As(err, &gerr) && gerr.Code == http.StatusForbidden) || (lookupCtx.Err() != nil && ctx.Err() == nil) {
			err = fmt.Errorf("%w: %w", errDirectoryLookupRefused, err)
		}
		return nil, directoryScopeError(fmt.Sprintf("getting user %s", userKey), err, directoryUserReadonlyScope)
	}

	return user, nil
}

// errDirectoryLookupRefused means that the Directory API refused a user
// lookup (403), or did not answer within licenseDirectoryLookupTimeout.
var errDirectoryLookupRefused = errors.New("the Directory API refused the user lookup or did not answer in time")

// errDirectoryLookupSkipped means that an optional lookup was not done,
// because an earlier one in the same provider configuration was refused.
var errDirectoryLookupSkipped = errors.New("an earlier Directory API user lookup of this provider configuration was " +
	"refused, so optional user lookups are skipped")

// Values of directoryLookupState.state.
const (
	directoryLookupsUnknown = iota
	directoryLookupsWork
	directoryLookupsRefused
)

// optionalLicenseDirectoryUser is getLicenseDirectoryUser for a lookup that
// the resource can do without (storing directory_user_id, and a refresh
// without it). After a lookup is refused, the optional lookups of the same
// provider configuration are skipped. Until the first lookup gives a result,
// lookups are done one at a time, so that concurrent refreshes make only one
// refused lookup. A client without directoryLookups (not made by configure)
// does every lookup.
func optionalLicenseDirectoryUser(ctx context.Context, client *apiClient, userKey string) (*directory.User, error) {
	st := client.directoryLookups
	if st == nil {
		return getLicenseDirectoryUser(ctx, client, userKey)
	}

	st.mu.Lock()
	switch st.state {
	case directoryLookupsRefused:
		st.mu.Unlock()
		return nil, errDirectoryLookupSkipped
	case directoryLookupsWork:
		st.mu.Unlock()
		user, err := getLicenseDirectoryUser(ctx, client, userKey)
		if errors.Is(err, errDirectoryLookupRefused) {
			st.mu.Lock()
			st.state = directoryLookupsRefused
			st.mu.Unlock()
			log.Printf("[WARN] License Assignment: skipping optional Directory API user lookups for the rest of this provider configuration: %s", err)
		}
		return user, err
	}
	defer st.mu.Unlock()

	user, err := getLicenseDirectoryUser(ctx, client, userKey)
	switch {
	case errors.Is(err, errDirectoryLookupRefused):
		st.state = directoryLookupsRefused
		log.Printf("[WARN] License Assignment: skipping optional Directory API user lookups for the rest of this provider configuration: %s", err)
	case err == nil:
		st.state = directoryLookupsWork
	}
	return user, err
}

func directoryUserPrimaryEmail(user *directory.User, fallback string) string {
	if user.PrimaryEmail == "" {
		return fallback
	}
	return user.PrimaryEmail
}

// licenseOwnerEmail returns the current primary email address of the user
// with the immutable ID storedUserId (the directory_user_id). Because the
// lookup is by ID, it follows a rename, also when the old address is no longer
// an alias, and never finds a different user that now has the old address.
// exists is false if the user does not exist.
func licenseOwnerEmail(ctx context.Context, client *apiClient, storedUserId, userId string) (primaryEmail string, exists bool, err error) {
	user, err := getLicenseDirectoryUser(ctx, client, storedUserId)
	if err != nil || user == nil {
		return "", false, err
	}
	if user.Id != storedUserId {
		// Never act on a different user.
		return "", false, fmt.Errorf("the Directory API returned user %s for directory_user_id %s", user.Id, storedUserId)
	}

	return directoryUserPrimaryEmail(user, userId), true, nil
}

// licenseWriteEmail returns the address for a write (a patch or a delete) to
// the license of the user. With directory_user_id (storedUserId) set, it is the
// current primary address of that user, errLicenseUserGone if the user does
// not exist, or the lookup error. Without directory_user_id, it is userId.
func licenseWriteEmail(ctx context.Context, client *apiClient, userId, storedUserId string) (string, error) {
	if storedUserId == "" {
		return userId, nil
	}

	primaryEmail, exists, err := licenseOwnerEmail(ctx, client, storedUserId, userId)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("%w (directory_user_id %s, user_id %s)", errLicenseUserGone, storedUserId, userId)
	}
	if !strings.EqualFold(primaryEmail, userId) {
		log.Printf("[DEBUG] License Assignment: user %s now has the primary email %s", userId, primaryEmail)
	}

	return primaryEmail, nil
}

// waitForLicenseAssignment waits until a get of the license assignment does
// not return 404, so that the read after a create or an update does not
// remove a license assignment that is not visible yet.
func waitForLicenseAssignment(ctx context.Context, licenseAssignmentsService *licensing.LicenseAssignmentsService, productId, skuId, userKey string, timeout time.Duration) error {
	return resource.RetryContext(ctx, timeout, func() *resource.RetryError {
		_, err := licenseAssignmentsService.Get(productId, skuId, userKey).Context(ctx).Do()
		if err == nil {
			return nil
		}
		if isApiErrorWithCode(err, http.StatusNotFound) {
			return resource.RetryableError(err)
		}

		return resource.NonRetryableError(err)
	})
}

// readLicenseAssignmentAfterWrite reads the license assignment after a create
// or an update. userKey is the user address that the write used. Unlike a
// refresh, it returns an error and keeps the ID if the license assignment is
// not found.
func readLicenseAssignmentAfterWrite(ctx context.Context, d *schema.ResourceData, meta interface{}, licenseAssignmentsService *licensing.LicenseAssignmentsService, userKey string, timeout time.Duration) diag.Diagnostics {
	id := d.Id()
	productId, skuId, _, err := parseLicenseAssignmentId(id)
	if err != nil {
		return diag.FromErr(err)
	}

	if err := waitForLicenseAssignment(ctx, licenseAssignmentsService, productId, skuId, userKey, timeout); err != nil {
		return diag.Errorf("License Assignment %s was written, but it could not be read: %s", id, err)
	}

	diags := readLicenseAssignment(ctx, d, meta, false)
	if d.Id() == "" {
		d.SetId(id)
		return append(diags, diag.Errorf("License Assignment %s was written, but it was not found when it was read", id)...)
	}

	return diags
}

func resourceLicenseAssignmentCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	productId := d.Get("product_id").(string)
	skuId := d.Get("sku_id").(string)
	userId := d.Get("user_id").(string)

	log.Printf("[DEBUG] Creating License Assignment product:%s, sku:%s, user:%s", productId, skuId, userId)

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return diags
	}

	licenseAssignmentsService, diags := GetLicenseAssignmentsService(licensingService)
	if diags.HasError() {
		return diags
	}

	err := resource.RetryContext(ctx, d.Timeout(schema.TimeoutCreate), func() *resource.RetryError {
		_, insertErr := licenseAssignmentsService.Insert(productId, skuId, &licensing.LicenseAssignmentInsert{
			UserId: userId,
		}).Context(withNoRetryOnPreconditionFailed(ctx)).Do()
		if insertErr == nil {
			return nil
		}
		if isLicenseUserNotReady(insertErr) {
			log.Printf("[DEBUG] License Assignment user %s is not ready yet, retrying: %s", userId, insertErr)
			return resource.RetryableError(insertErr)
		}

		return resource.NonRetryableError(licenseInsertError(insertErr, productId, skuId, userId))
	})
	if err != nil {
		return diag.FromErr(err)
	}

	d.SetId(licenseAssignmentId(productId, skuId, userId))

	log.Printf("[DEBUG] Finished creating License Assignment %q", d.Id())

	return readLicenseAssignmentAfterWrite(ctx, d, meta, licenseAssignmentsService, userId, d.Timeout(schema.TimeoutCreate))
}

// LicensingCustomerId returns the customer ID for the License Manager API list
// methods. The API reference documents customerId as the customer's unique ID,
// such as C00000000, and does not document the "my_customer" alias that the
// Directory API accepts. If customer_id is "my_customer", the unique ID is read
// from the Directory API. A client from configure caches it; a client without
// a cache reads it each time.
func (c *apiClient) LicensingCustomerId(ctx context.Context) (string, diag.Diagnostics) {
	if c.Customer != "my_customer" {
		return c.Customer, nil
	}

	if c.licensingCustomer != nil {
		c.licensingCustomer.mu.Lock()
		defer c.licensingCustomer.mu.Unlock()

		if c.licensingCustomer.id != "" {
			return c.licensingCustomer.id, nil
		}
		// The context can have ended while this call waited for the lock.
		if err := ctx.Err(); err != nil {
			return "", diag.FromErr(err)
		}
	}

	directoryService, diags := c.NewDirectoryService()
	if diags.HasError() {
		return "", diags
	}

	lookupCtx, cancel := context.WithTimeout(ctx, licenseDirectoryLookupTimeout)
	defer cancel()

	customer, err := directoryService.Customers.Get(c.Customer).Context(lookupCtx).Do()
	if err != nil {
		return "", diag.Errorf("%s. Or set customer_id to the unique customer ID, such as C01234567",
			directoryScopeError(fmt.Sprintf("getting the unique customer ID for customer_id %q", c.Customer), err, directoryCustomerReadonlyScope))
	}
	if customer.Id == "" {
		return "", diag.Errorf("The Directory API returned no customer ID for customer_id %q", c.Customer)
	}

	if c.licensingCustomer != nil {
		c.licensingCustomer.id = customer.Id
	}
	return customer.Id, nil
}

// licenseAssignmentsOfProduct lists all license assignments of a product. If
// useCache is true and the client has a cache (a client from configure), the
// list is read once for each product and customer and shared by all calls of
// the provider configuration.
func (c *apiClient) licenseAssignmentsOfProduct(ctx context.Context, licenseAssignmentsService *licensing.LicenseAssignmentsService, customerId, productId string, useCache bool) ([]*licensing.LicenseAssignment, error) {
	key := productId + licenseAssignmentIdSeparator + customerId
	cache := c.licenseAssignmentLists
	if useCache && cache != nil {
		cache.mu.Lock()
		defer cache.mu.Unlock()

		if list, ok := cache.lists[key]; ok {
			return list, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	log.Printf("[INFO] Listing the license assignments of product %s", productId)

	var list []*licensing.LicenseAssignment
	err := licenseAssignmentsService.ListForProduct(productId, customerId).MaxResults(licenseAssignmentsMaxResults).
		Pages(ctx, func(resp *licensing.LicenseAssignmentList) error {
			list = append(list, resp.Items...)
			return nil
		})
	if err != nil {
		return nil, err
	}

	if useCache && cache != nil {
		if cache.lists == nil {
			cache.lists = map[string][]*licensing.LicenseAssignment{}
		}
		cache.lists[key] = list
	}
	return list, nil
}

// findLicenseAssignmentForUser returns the license assignment of the user in
// the list, with a case-insensitive match of the address, or nil.
func findLicenseAssignmentForUser(list []*licensing.LicenseAssignment, userId string) *licensing.LicenseAssignment {
	for _, la := range list {
		if la != nil && strings.EqualFold(la.UserId, userId) {
			return la
		}
	}
	return nil
}

func resourceLicenseAssignmentRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	return readLicenseAssignment(ctx, d, meta, true)
}

// readLicenseAssignment reads the license assignment of the ID. The ID and
// user_id keep the configured address.
//
// With directory_user_id set, the user is found by that ID first, and the
// license is read once with the user's current primary address. If the user
// does not exist, the resource is removed from state. If the lookup fails, the
// address in user_id is used, and a license that is not found is an error.
//
// Without directory_user_id, the license is read with user_id. If it is found,
// directory_user_id is stored when the Directory API confirms that user_id is
// the user's primary address (best effort). If it is not found, the user is
// looked up: the resource is removed from state if the user does not exist or
// is found only through an alias. If the lookup fails, only the License
// Manager API is used.
//
// For a refresh (refresh true), a license that is not found for an existing
// user is searched in the license assignments of the product. If the user has
// another SKU, sku_id and the ID are set to it, so that the plan changes
// sku_id back in place. The resource is removed from state only if the user
// has no license for the product.
func readLicenseAssignment(ctx context.Context, d *schema.ResourceData, meta interface{}, refresh bool) diag.Diagnostics {
	client := meta.(*apiClient)

	productId, skuId, userId, err := parseLicenseAssignmentId(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return diags
	}

	licenseAssignmentsService, diags := GetLicenseAssignmentsService(licensingService)
	if diags.HasError() {
		return diags
	}

	log.Printf("[DEBUG] Getting License Assignment %q", d.Id())

	var la *licensing.LicenseAssignment
	get := func(userKey string) (e error) {
		la, e = licenseAssignmentsService.Get(productId, skuId, userKey).Context(ctx).Do()
		return
	}

	directoryUserId := d.Get("directory_user_id").(string)
	userKey := userId
	var ownerErr error

	if directoryUserId != "" {
		primaryEmail, exists, lookupErr := licenseOwnerEmail(ctx, client, directoryUserId, userId)
		switch {
		case lookupErr != nil:
			log.Printf("[WARN] License Assignment %s: cannot find user %s, using user_id: %s", d.Id(), directoryUserId, lookupErr)
			ownerErr = lookupErr
		case !exists:
			log.Printf("[WARN] Removing License Assignment %s: %s", d.Id(), errLicenseUserGone)
			d.SetId("")
			return nil
		default:
			userKey = primaryEmail
			if refresh && !strings.EqualFold(primaryEmail, userId) {
				diags = append(diags, diag.Diagnostic{
					Severity: diag.Warning,
					Summary:  "The user of the license assignment was renamed",
					Detail: fmt.Sprintf("The user of License Assignment %s now has the primary email address %s. "+
						"The provider uses that address. Set user_id to %s.", d.Id(), primaryEmail, primaryEmail),
				})
			}
		}
	}

	if directoryUserId != "" && ownerErr == nil {
		// The address is the owner's current primary address: one read, no wait.
		err = get(userKey)
	} else if refresh {
		err = retryNotFound(ctx, func() error { return get(userKey) })
	} else {
		err = get(userKey)
	}

	if err == nil && directoryUserId == "" {
		// The License Manager API accepts only the user's primary address, so
		// user_id is this user's address: store the user's immutable ID. This
		// is best effort.
		user, lookupErr := optionalLicenseDirectoryUser(ctx, client, userId)
		if lookupErr != nil {
			log.Printf("[WARN] License Assignment %s: cannot store directory_user_id: %s", d.Id(), lookupErr)
		} else if user != nil && strings.EqualFold(directoryUserPrimaryEmail(user, userId), userId) {
			directoryUserId = user.Id
		}
	}

	if isLicenseUserNotReady(err) {
		if ownerErr != nil {
			return diag.Errorf("Error reading License Assignment %s: the license was not found with %s, and the user "+
				"could not be found by directory_user_id: %s", d.Id(), userKey, ownerErr)
		}

		if directoryUserId == "" {
			user, lookupErr := optionalLicenseDirectoryUser(ctx, client, userId)
			switch {
			case lookupErr != nil:
				// Without directory_user_id, the Directory API is optional:
				// continue with the License Manager API only.
				log.Printf("[WARN] License Assignment %s: cannot get the user, so a rename is not followed: %s", d.Id(), lookupErr)
			case user == nil:
				log.Printf("[WARN] Removing License Assignment %s because the user does not exist", d.Id())
				d.SetId("")
				return nil
			case !strings.EqualFold(directoryUserPrimaryEmail(user, userId), userId):
				log.Printf("[WARN] Removing License Assignment %s: %s is an alias of %s, and directory_user_id is not set "+
					"to confirm the user", d.Id(), userId, directoryUserPrimaryEmail(user, userId))
				d.SetId("")
				return nil
			default:
				directoryUserId = user.Id
			}
		}

		if refresh {
			customerId, diags := client.LicensingCustomerId(ctx)
			if diags.HasError() {
				return diags
			}

			list, listErr := client.licenseAssignmentsOfProduct(ctx, licenseAssignmentsService, customerId, productId, true)
			if listErr != nil {
				return diag.Errorf("Error finding the license of user %s for product %s: %s", userKey, productId, listErr)
			}
			la = findLicenseAssignmentForUser(list, userKey)
			if la == nil {
				log.Printf("[WARN] Removing License Assignment %s because the user has no license for the product", d.Id())
				d.SetId("")
				return nil
			}

			log.Printf("[WARN] License Assignment %s: the user has sku %s of the product instead", d.Id(), la.SkuId)
			skuId = la.SkuId
			d.SetId(licenseAssignmentId(productId, skuId, userId))
			err = nil
		}
	}
	if err != nil {
		return handleNotFoundError(err, d, d.Id())
	}
	if la == nil {
		return diag.Errorf("No License Assignment was returned for %s.", d.Id())
	}

	d.Set("product_id", productId)
	d.Set("sku_id", skuId)
	d.Set("user_id", userId)
	d.Set("directory_user_id", directoryUserId)
	d.Set("etag", la.Etags)
	d.Set("product_name", la.ProductName)
	d.Set("sku_name", la.SkuName)
	d.Set("self_link", la.SelfLink)

	log.Printf("[DEBUG] Finished getting License Assignment %q", d.Id())

	return diags
}

func resourceLicenseAssignmentUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	// On an error, keep the state that was there before the update.
	failed := func(diags diag.Diagnostics) diag.Diagnostics {
		d.Partial(true)
		return diags
	}

	oldId := d.Id()
	productId, oldSkuId, _, err := parseLicenseAssignmentId(oldId)
	if err != nil {
		return failed(diag.FromErr(err))
	}

	// A change to user_id is in place only if resourceLicenseAssignmentCustomizeDiff
	// confirmed that the new value is the same user with this license. The
	// change then needs no API write: the ID gets the new value.
	userId := d.Get("user_id").(string)
	newSkuId := d.Get("sku_id").(string)

	if !d.HasChange("sku_id") {
		d.SetId(licenseAssignmentId(productId, newSkuId, userId))
		diags := readLicenseAssignment(ctx, d, meta, false)
		if d.Id() == "" {
			d.SetId(oldId)
			return failed(append(diags, diag.Errorf("The license assignment no longer exists for user %s (product %s, sku %s)",
				userId, productId, newSkuId)...))
		}
		if diags.HasError() {
			d.SetId(oldId)
			return failed(diags)
		}
		return diags
	}

	log.Printf("[DEBUG] Updating License Assignment %q to sku:%s, user:%s", d.Id(), newSkuId, userId)

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return failed(diags)
	}

	licenseAssignmentsService, diags := GetLicenseAssignmentsService(licensingService)
	if diags.HasError() {
		return failed(diags)
	}

	userKey, err := licenseWriteEmail(ctx, client, userId, d.Get("directory_user_id").(string))
	if err != nil {
		return failed(diag.Errorf("Error reassigning license (product %s) of user %s from sku %s to sku %s: %s. "+
			"The license was not changed", productId, userId, oldSkuId, newSkuId, err))
	}

	// The current SKU is in the request path and the new SKU is in the request body.
	// A 412 is not retried: see licensePatchError.
	_, err = licenseAssignmentsService.Patch(productId, oldSkuId, userKey, &licensing.LicenseAssignment{
		SkuId: newSkuId,
	}).Context(withNoRetryOnPreconditionFailed(ctx)).Do()
	if err != nil {
		return failed(diag.FromErr(licensePatchError(err, productId, oldSkuId, newSkuId, userId)))
	}

	d.SetId(licenseAssignmentId(productId, newSkuId, userId))

	log.Printf("[DEBUG] Finished updating License Assignment %q", d.Id())

	diags = readLicenseAssignmentAfterWrite(ctx, d, meta, licenseAssignmentsService, userKey, d.Timeout(schema.TimeoutUpdate))
	if diags.HasError() {
		// The patch was done: keep the new ID, but not the other planned values.
		d.Partial(true)
	}
	return diags
}

// isSameLicensedUser reports whether newUserId is the current primary address
// of the user of the license assignment (storedUserId, the directory_user_id)
// and has the license assignment of the product and SKU.
//
// It returns false with no error only for a definite no: directory_user_id is
// not set, the new user is not found, the user IDs differ, or the License
// Manager API does not find the license for the new address. An address that
// is an alias of the user is an error, because user_id must be the primary
// address. Any other error is returned, so that the plan fails instead of
// replacing the license assignment because of, for example, a missing scope.
func isSameLicensedUser(ctx context.Context, client *apiClient, productId, skuId, storedUserId, newUserId string) (bool, error) {
	if storedUserId == "" {
		log.Printf("[DEBUG] License Assignment: directory_user_id is not set, so a change to user %s cannot be confirmed", newUserId)
		return false, nil
	}

	user, err := getLicenseDirectoryUser(ctx, client, newUserId)
	if err != nil {
		return false, err
	}
	if user == nil {
		log.Printf("[DEBUG] License Assignment: user %s does not exist", newUserId)
		return false, nil
	}
	if user.Id != storedUserId {
		return false, nil
	}
	if primaryEmail := directoryUserPrimaryEmail(user, newUserId); !strings.EqualFold(primaryEmail, newUserId) {
		return false, fmt.Errorf("user_id must be the user's primary email address (%s), not %s", primaryEmail, newUserId)
	}

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return false, fmt.Errorf("%s", diags[0].Summary)
	}
	_, err = licensingService.LicenseAssignments.Get(productId, skuId, newUserId).Context(ctx).Do()
	if isLicenseUserNotReady(err) {
		log.Printf("[DEBUG] License Assignment: user %s has no license for product %s, sku %s: %s", newUserId, productId, skuId, err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("Error checking the license of user %s for product %s, sku %s: %w. This needs the %s scope",
			newUserId, productId, skuId, err, licensingClientScope)
	}

	return true, nil
}

// resourceLicenseAssignmentCustomizeDiff replaces the license assignment on a
// change to user_id, except when isSameLicensedUser confirms that the new
// value is the same user with the same license (for example after a rename of
// the user's primary email address outside this resource).
func resourceLicenseAssignmentCustomizeDiff(ctx context.Context, diff *schema.ResourceDiff, meta interface{}) error {
	if diff.Id() == "" || !diff.HasChange("user_id") || diff.HasChange("product_id") {
		return nil
	}
	if !diff.NewValueKnown("user_id") {
		return diff.ForceNew("user_id")
	}

	oldValue, newValue := diff.GetChange("user_id")
	oldUserId, newUserId := oldValue.(string), newValue.(string)
	if strings.EqualFold(oldUserId, newUserId) {
		return nil
	}

	client, ok := meta.(*apiClient)
	if !ok || client == nil {
		return diff.ForceNew("user_id")
	}

	productId, skuId, _, err := parseLicenseAssignmentId(diff.Id())
	if err != nil {
		return diff.ForceNew("user_id")
	}

	same, err := isSameLicensedUser(ctx, client, productId, skuId, diff.Get("directory_user_id").(string), newUserId)
	if err != nil {
		return err
	}
	if same {
		log.Printf("[DEBUG] License Assignment %s: user_id changes in place from %s to %s", diff.Id(), oldUserId, newUserId)
		return nil
	}

	return diff.ForceNew("user_id")
}

func resourceLicenseAssignmentDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	productId, skuId, userId, err := parseLicenseAssignmentId(d.Id())
	if err != nil {
		return diag.FromErr(err)
	}

	log.Printf("[DEBUG] Deleting License Assignment %q", d.Id())

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return diags
	}

	licenseAssignmentsService, diags := GetLicenseAssignmentsService(licensingService)
	if diags.HasError() {
		return diags
	}

	directoryUserId := d.Get("directory_user_id").(string)
	userKey, err := licenseWriteEmail(ctx, client, userId, directoryUserId)
	if errors.Is(err, errLicenseUserGone) {
		// A deleted user has no licenses. The address can belong to a
		// different user now, whose license must not be removed.
		log.Printf("[WARN] License Assignment %q is already gone: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	if err != nil {
		return diag.Errorf("Error deleting License Assignment %s: %s", d.Id(), err)
	}

	deleteSku := func(sku string) error {
		_, e := licenseAssignmentsService.Delete(productId, sku, userKey).Context(ctx).Do()
		return e
	}
	err = deleteSku(skuId)

	if isLicenseUserNotReady(err) && directoryUserId != "" {
		// The SKU can have changed outside Terraform since the last refresh.
		// userKey is the address of the user in directory_user_id, so this
		// finds only that user's license.
		// If the search fails, the license is treated as gone, as without
		// directory_user_id: the owner was confirmed, and the license was not
		// found with the stored SKU.
		customerId, customerDiags := client.LicensingCustomerId(ctx)
		var list []*licensing.LicenseAssignment
		var listErr error
		if customerDiags.HasError() {
			listErr = fmt.Errorf("%s", customerDiags[0].Summary)
		} else {
			list, listErr = client.licenseAssignmentsOfProduct(ctx, licenseAssignmentsService, customerId, productId, false)
		}
		if listErr != nil {
			log.Printf("[WARN] License Assignment %q: cannot search the user's current SKU, treating the license as gone: %s", d.Id(), listErr)
		} else if la := findLicenseAssignmentForUser(list, userKey); la != nil {
			log.Printf("[WARN] License Assignment %q: deleting the user's current sku %s", d.Id(), la.SkuId)
			err = deleteSku(la.SkuId)
		}
	}

	if isLicenseUserNotReady(err) {
		// Without directory_user_id, a license with a SKU that changed outside
		// Terraform is not searched, and is treated as gone too.
		log.Printf("[WARN] License Assignment %q is already gone: %s", d.Id(), err)
		d.SetId("")
		return nil
	}
	if err != nil {
		return handleNotFoundError(err, d, d.Id())
	}

	log.Printf("[DEBUG] Finished deleting License Assignment %q", d.Id())

	return diags
}

func resourceLicenseAssignmentImport(ctx context.Context, d *schema.ResourceData, meta interface{}) ([]*schema.ResourceData, error) {
	if _, _, _, err := parseLicenseAssignmentId(d.Id()); err != nil {
		return nil, err
	}

	return []*schema.ResourceData{d}, nil
}
