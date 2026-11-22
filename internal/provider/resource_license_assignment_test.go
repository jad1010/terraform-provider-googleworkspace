// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// The license acceptance tests assign a real license. Set these variables to
// a product and SKUs that have free licenses in the test domain:
//   - GOOGLEWORKSPACE_LICENSE_PRODUCT_ID
//   - GOOGLEWORKSPACE_LICENSE_SKU_ID
//   - GOOGLEWORKSPACE_LICENSE_SKU_ID_UPDATE (a different SKU of the same
//     product, only for TestAccResourceLicenseAssignment_updateSku)
//
// The tests create new users. Disable automatic license assignment for the
// product in the test domain, because a license that Google Workspace assigns
// automatically causes the insert to fail.
func testAccLicenseAssignmentData(t *testing.T) map[string]interface{} {
	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")
	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	productId := os.Getenv("GOOGLEWORKSPACE_LICENSE_PRODUCT_ID")
	skuId := os.Getenv("GOOGLEWORKSPACE_LICENSE_SKU_ID")
	if productId == "" || skuId == "" {
		t.Skip("GOOGLEWORKSPACE_LICENSE_PRODUCT_ID and GOOGLEWORKSPACE_LICENSE_SKU_ID need to be set to run this test")
	}

	return map[string]interface{}{
		"domainName":  domainName,
		"userEmail":   fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"password":    acctest.RandString(10),
		"productId":   productId,
		"skuId":       skuId,
		"oauthScopes": testAccLicensingOauthScopes(),
	}
}

// testAccLicensingOauthScopes returns the default scopes and the licensing
// scope as an HCL list, because the licensing scope is not a default scope.
func testAccLicensingOauthScopes() string {
	scopes := append(append([]string{}, DefaultClientScopes...), licensingClientScope)
	return `["` + strings.Join(scopes, `", "`) + `"]`
}

func TestAccResourceLicenseAssignment_basic(t *testing.T) {
	t.Parallel()

	data := testAccLicenseAssignmentData(t)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testAccCheckLicenseAssignmentDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccLicenseAssignment(data),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "product_id", data["productId"].(string)),
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "sku_id", data["skuId"].(string)),
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "user_id",
						fmt.Sprintf("%s@%s", data["userEmail"], data["domainName"])),
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "id",
						fmt.Sprintf("%s/%s/%s@%s", data["productId"], data["skuId"], data["userEmail"], data["domainName"])),
					resource.TestCheckResourceAttrSet("googleworkspace_license_assignment.test", "sku_name"),
					resource.TestCheckResourceAttrSet("googleworkspace_license_assignment.test", "product_name"),
					resource.TestCheckResourceAttrSet("googleworkspace_license_assignment.test", "directory_user_id"),
				),
			},
			{
				ResourceName:            "googleworkspace_license_assignment.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"etag"},
			},
		},
	})
}

func TestAccResourceLicenseAssignment_updateSku(t *testing.T) {
	t.Parallel()

	data := testAccLicenseAssignmentData(t)

	updatedSkuId := os.Getenv("GOOGLEWORKSPACE_LICENSE_SKU_ID_UPDATE")
	if updatedSkuId == "" {
		t.Skip("GOOGLEWORKSPACE_LICENSE_SKU_ID_UPDATE needs to be set to run this test")
	}

	updatedData := make(map[string]interface{}, len(data))
	for k, v := range data {
		updatedData[k] = v
	}
	updatedData["skuId"] = updatedSkuId

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testAccCheckLicenseAssignmentDestroy,
		Steps: []resource.TestStep{
			{
				Config: testAccLicenseAssignment(data),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "sku_id", data["skuId"].(string)),
				),
			},
			{
				Config: testAccLicenseAssignment(updatedData),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "sku_id", updatedSkuId),
					resource.TestCheckResourceAttr("googleworkspace_license_assignment.test", "id",
						fmt.Sprintf("%s/%s/%s@%s", data["productId"], updatedSkuId, data["userEmail"], data["domainName"])),
				),
			},
			{
				ResourceName:            "googleworkspace_license_assignment.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"etag"},
			},
		},
	})
}

// testAccCheckLicenseAssignmentDestroy checks that each license assignment in
// the state is gone. The tests also delete the user, so a 400 "User email not
// valid" error also counts as gone.
func testAccCheckLicenseAssignmentDestroy(s *terraform.State) error {
	client, err := googleworkspaceTestClient()
	if err != nil {
		return err
	}
	client.ClientScopes = append(append([]string{}, DefaultClientScopes...), licensingClientScope)
	if diags := client.loadAndValidate(context.Background()); diags.HasError() {
		return fmt.Errorf("%s", diags[0].Summary)
	}

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return fmt.Errorf("%s", diags[0].Summary)
	}

	for _, rs := range s.RootModule().Resources {
		if rs.Type != "googleworkspace_license_assignment" {
			continue
		}

		productId, skuId, userId, err := parseLicenseAssignmentId(rs.Primary.ID)
		if err != nil {
			return err
		}

		_, err = licensingService.LicenseAssignments.Get(productId, skuId, userId).Do()
		if err == nil {
			return fmt.Errorf("license assignment %s still exists", rs.Primary.ID)
		}
		if !isLicenseUserNotReady(err) {
			return fmt.Errorf("unexpected error when checking license assignment %s: %s", rs.Primary.ID, err)
		}
	}

	return nil
}

func testAccLicenseAssignment(data map[string]interface{}) string {
	return Nprintf(`
provider "googleworkspace" {
  oauth_scopes = %{oauthScopes}
}

resource "googleworkspace_user" "test" {
  primary_email = "%{userEmail}@%{domainName}"
  password      = "%{password}"

  name {
    family_name = "Scott"
    given_name  = "Michael"
  }
}

resource "googleworkspace_license_assignment" "test" {
  product_id = "%{productId}"
  sku_id     = "%{skuId}"
  user_id    = googleworkspace_user.test.primary_email
}
`, data)
}
