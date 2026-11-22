// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"google.golang.org/api/licensing/v1"
)

// licenseAssignmentsMaxResults is the maximum page size of the list methods,
// from the API discovery document (the default is 100).
const licenseAssignmentsMaxResults = 1000

func dataSourceLicenseAssignments() *schema.Resource {
	return &schema.Resource{
		// This description is used by the documentation generator and the language server.
		Description: "License Assignments data source in the Terraform Googleworkspace provider. It lists the " +
			"users that have a license for a product, or for one SKU of a product.\n\n" +
			"This data source uses these OAuth scopes. The provider's default scopes do not include `" +
			licensingClientScope + "`, and `oauth_scopes` replaces the default list, so set `oauth_scopes` to " +
			"all the scopes that your configuration needs:\n\n" +
			"- `" + licensingClientScope + "` (always).\n" +
			"- `" + directoryCustomerReadonlyScope + "` or `https://www.googleapis.com/auth/admin.directory.customer` " +
			"(only if `customer_id` is `my_customer`).\n\n" +
			"The list requests send the provider's `customer_id` as the `customerId` parameter. The API reference " +
			"documents this parameter as the customer's unique ID, such as `C00000000`, and the API guide shows the " +
			"customer's primary domain name. Neither documents the `my_customer` alias. If `customer_id` is " +
			"`my_customer`, the provider gets the unique customer ID once from the Directory API and sends that ID.",

		ReadContext: dataSourceLicenseAssignmentsRead,

		Schema: map[string]*schema.Schema{
			"product_id": {
				Description: "The product's unique identifier, for example `Google-Apps`. " +
					"See [Products and SKUs](https://developers.google.com/workspace/admin/licensing/v1/how-tos/products).",
				Type:     schema.TypeString,
				Required: true,
			},
			"sku_id": {
				Description: "The product SKU's unique identifier. If set, only the license assignments " +
					"of this SKU are listed. If not set, the license assignments of all SKUs of the product are listed.",
				Type:     schema.TypeString,
				Optional: true,
			},
			"license_assignments": {
				Description: "A list of license assignments.",
				Type:        schema.TypeList,
				Computed:    true,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"user_id": {
							Description: "The user's primary email address.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"product_id": {
							Description: "The product's unique identifier.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"sku_id": {
							Description: "The product SKU's unique identifier.",
							Type:        schema.TypeString,
							Computed:    true,
						},
						"sku_name": {
							Description: "Display name of the product SKU.",
							Type:        schema.TypeString,
							Computed:    true,
						},
					},
				},
			},
		},
	}
}

func dataSourceLicenseAssignmentsRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*apiClient)

	licensingService, diags := client.NewLicensingService()
	if diags.HasError() {
		return diags
	}

	licenseAssignmentsService, diags := GetLicenseAssignmentsService(licensingService)
	if diags.HasError() {
		return diags
	}

	customerId, diags := client.LicensingCustomerId(ctx)
	if diags.HasError() {
		return diags
	}

	productId := d.Get("product_id").(string)
	skuId := d.Get("sku_id").(string)

	var result []*licensing.LicenseAssignment
	collect := func(resp *licensing.LicenseAssignmentList) error {
		result = append(result, resp.Items...)
		return nil
	}

	var err error
	if skuId == "" {
		err = licenseAssignmentsService.ListForProduct(productId, customerId).
			MaxResults(licenseAssignmentsMaxResults).Pages(ctx, collect)
	} else {
		err = licenseAssignmentsService.ListForProductAndSku(productId, skuId, customerId).
			MaxResults(licenseAssignmentsMaxResults).Pages(ctx, collect)
	}
	if err != nil {
		return diag.Errorf("Error listing license assignments for product %s: %s", productId, err)
	}

	if err := d.Set("license_assignments", flattenLicenseAssignments(result)); err != nil {
		return diag.FromErr(err)
	}

	if skuId == "" {
		d.SetId(productId)
	} else {
		d.SetId(productId + licenseAssignmentIdSeparator + skuId)
	}

	return diags
}

func flattenLicenseAssignments(licenseAssignments []*licensing.LicenseAssignment) []interface{} {
	result := make([]interface{}, 0, len(licenseAssignments))

	for _, la := range licenseAssignments {
		if la == nil {
			continue
		}
		result = append(result, map[string]interface{}{
			"user_id":    la.UserId,
			"product_id": la.ProductId,
			"sku_id":     la.SkuId,
			"sku_name":   la.SkuName,
		})
	}

	return result
}
