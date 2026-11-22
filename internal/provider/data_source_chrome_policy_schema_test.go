// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestAccDataSourceChromePolicySchema(t *testing.T) {
	t.Parallel()

	schemaName := "chrome.printers.AllowForUsers"

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccDataSourceChromePolicySchema(schemaName),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_policy_schema.test", "schema_name", schemaName),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_policy_schema.test", "policy_description", "Allows a printer for users in a given organization."),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_policy_schema.test", "additional_target_key_names.#", "1"),
					resource.TestCheckResourceAttr("data.googleworkspace_chrome_policy_schema.test", "additional_target_key_names.0.key", "printer_id"),
					resource.TestCheckResourceAttrSet("data.googleworkspace_chrome_policy_schema.test", "supported_platforms.#"),
					resource.TestCheckResourceAttrSet("data.googleworkspace_chrome_policy_schema.test", "enum_fields.#"),
				),
			},
		},
	})
}

func testAccDataSourceChromePolicySchema(schemaName string) string {
	return fmt.Sprintf(`
data "googleworkspace_chrome_policy_schema" "test" {
  schema_name = "%s"
}
`, schemaName)
}

func TestAccDataSourceChromePolicySchema_enumFields(t *testing.T) {
	t.Parallel()

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccDataSourceChromePolicySchema("chrome.users.IncognitoMode"),
				Check: testAccCheckChromePolicySchemaEnumValue("data.googleworkspace_chrome_policy_schema.test",
					"incognitoModeAvailability", "_ENUM_UNAVAILABLE"),
			},
		},
	})
}

// testAccCheckChromePolicySchemaEnumValue checks that enum_fields has the field, with a value whose name ends with
// suffix and has a description.
func testAccCheckChromePolicySchemaEnumValue(name, field, suffix string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("%s not found in state", name)
		}
		attrs := rs.Primary.Attributes

		for i := 0; attrs[fmt.Sprintf("enum_fields.%d.field", i)] != ""; i++ {
			if attrs[fmt.Sprintf("enum_fields.%d.field", i)] != field {
				continue
			}
			for j := 0; attrs[fmt.Sprintf("enum_fields.%d.values.%d.value", i, j)] != ""; j++ {
				value := attrs[fmt.Sprintf("enum_fields.%d.values.%d.value", i, j)]
				if !strings.HasSuffix(value, suffix) {
					continue
				}
				if attrs[fmt.Sprintf("enum_fields.%d.values.%d.description", i, j)] == "" {
					return fmt.Errorf("enum value %s of field %s has no description", value, field)
				}
				return nil
			}
			return fmt.Errorf("field %s has no enum value that ends with %s", field, suffix)
		}
		return fmt.Errorf("enum_fields has no field %s", field)
	}
}
