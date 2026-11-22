// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/licensing/v1"
)

func TestLicenseAssignmentId_roundTrip(t *testing.T) {
	cases := []struct {
		productId, skuId, userId string
	}{
		{"Google-Apps", "1010020027", "alice@example.com"},
		{"101031", "1010310008", "bob.o'neil@example.com"},
		{"Google-Drive-storage", "Google-Drive-storage-20GB", "123456789012345678901"},
	}

	for _, tc := range cases {
		id := licenseAssignmentId(tc.productId, tc.skuId, tc.userId)
		productId, skuId, userId, err := parseLicenseAssignmentId(id)
		if err != nil {
			t.Fatalf("parseLicenseAssignmentId(%q) returned error: %s", id, err)
		}
		if productId != tc.productId || skuId != tc.skuId || userId != tc.userId {
			t.Errorf("parseLicenseAssignmentId(%q) = (%q, %q, %q), want (%q, %q, %q)",
				id, productId, skuId, userId, tc.productId, tc.skuId, tc.userId)
		}
	}
}

func TestLicenseAssignmentId_format(t *testing.T) {
	got := licenseAssignmentId("Google-Apps", "1010020027", "alice@example.com")
	want := "Google-Apps/1010020027/alice@example.com"
	if got != want {
		t.Errorf("licenseAssignmentId() = %q, want %q", got, want)
	}
}

func TestParseLicenseAssignmentId_invalid(t *testing.T) {
	cases := []string{
		"",
		"Google-Apps",
		"Google-Apps/1010020027",
		"Google-Apps/1010020027/alice@example.com/extra",
		"/1010020027/alice@example.com",
		"Google-Apps//alice@example.com",
		"Google-Apps/1010020027/",
		"//",
	}

	for _, id := range cases {
		if _, _, _, err := parseLicenseAssignmentId(id); err == nil {
			t.Errorf("parseLicenseAssignmentId(%q) returned no error, want an error", id)
		}
	}
}

func TestIsLicenseUserNotReady(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"not found": {
			err:  &googleapi.Error{Code: 404, Message: "Not Found"},
			want: true,
		},
		"user email not valid": {
			err:  &googleapi.Error{Code: 400, Message: "User email not valid."},
			want: true,
		},
		"wrapped not found": {
			err:  fmt.Errorf("wrapped: %w", &googleapi.Error{Code: 404}),
			want: true,
		},
		"other bad request": {
			err:  &googleapi.Error{Code: 400, Message: "Invalid sku"},
			want: false,
		},
		"already assigned": {
			err:  &googleapi.Error{Code: 412, Message: "User already has a license for the specified product and SKU"},
			want: false,
		},
		"forbidden": {
			err:  &googleapi.Error{Code: 403, Message: "Request had insufficient authentication scopes."},
			want: false,
		},
		"not an api error": {
			err:  errors.New("connection reset"),
			want: false,
		},
		"nil": {
			err:  nil,
			want: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isLicenseUserNotReady(tc.err); got != tc.want {
				t.Errorf("isLicenseUserNotReady() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestFlattenLicenseAssignments(t *testing.T) {
	got := flattenLicenseAssignments([]*licensing.LicenseAssignment{
		{
			UserId:      "alice@example.com",
			ProductId:   "Google-Apps",
			ProductName: "Google Workspace",
			SkuId:       "1010020027",
			SkuName:     "Google Workspace Business Starter",
		},
		nil,
	})

	want := []interface{}{
		map[string]interface{}{
			"user_id":    "alice@example.com",
			"product_id": "Google-Apps",
			"sku_id":     "1010020027",
			"sku_name":   "Google Workspace Business Starter",
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("flattenLicenseAssignments() = %#v, want %#v", got, want)
	}

	if empty := flattenLicenseAssignments(nil); empty == nil || len(empty) != 0 {
		t.Errorf("flattenLicenseAssignments(nil) = %#v, want an empty list", empty)
	}
}

func TestLicensingClientScopeNotInDefaults(t *testing.T) {
	// Adding this scope to DefaultClientScopes breaks users of domain-wide
	// delegation that did not grant it. See the provider documentation.
	if stringInSlice(DefaultClientScopes, licensingClientScope) {
		t.Errorf("DefaultClientScopes must not contain %s", licensingClientScope)
	}
}

func TestLicenseInsertError(t *testing.T) {
	preconditionFailed := &googleapi.Error{Code: 412, Message: "User already has a license for the specified product and SKU"}
	err := licenseInsertError(preconditionFailed, "Google-Apps", "1010020027", "alice@example.com")

	for _, want := range []string{
		"User already has a license for the specified product and SKU",
		"already has a license for this product, possibly with a different SKU",
		"no free seats",
		"Google-Apps/1010020027/alice@example.com",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("licenseInsertError() = %q, want it to contain %q", err, want)
		}
	}
	if !errors.Is(err, preconditionFailed) {
		t.Errorf("licenseInsertError() does not wrap the API error")
	}

	other := &googleapi.Error{Code: 400, Message: "Invalid sku"}
	if got := licenseInsertError(other, "Google-Apps", "1010020027", "alice@example.com"); got != other {
		t.Errorf("licenseInsertError() changed a non-412 error: %q", got)
	}
}

func TestLicensePatchError(t *testing.T) {
	preconditionFailed := &googleapi.Error{Code: 412, Message: "There aren't enough available licenses for the specified product-SKU pair"}
	err := licensePatchError(preconditionFailed, "Google-Apps", "1010020027", "1010020028", "alice@example.com")

	for _, want := range []string{
		"There aren't enough available licenses for the specified product-SKU pair",
		"from sku 1010020027 to sku 1010020028",
		"no free seats",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("licensePatchError() = %q, want it to contain %q", err, want)
		}
	}
	if !errors.Is(err, preconditionFailed) {
		t.Errorf("licensePatchError() does not wrap the API error")
	}

	other := &googleapi.Error{Code: 404, Message: "Not Found"}
	if got := licensePatchError(other, "Google-Apps", "1010020027", "1010020028", "alice@example.com"); got != other {
		t.Errorf("licensePatchError() changed a non-412 error: %q", got)
	}
}
