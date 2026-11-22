// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestAccResourceGmailSendAsAlias_basic(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")

	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	data := map[string]interface{}{
		"domainName": domainName,
		"userEmail":  fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"password":   acctest.RandString(10),
		"gmailUser":  gmailUser,
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailSendAsAlias_basic(data),
			},
			{
				ResourceName:      "googleworkspace_gmail_send_as_alias.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccResourceGmailSendAsAlias_full(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")

	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	data := map[string]interface{}{
		"domainName": domainName,
		"userEmail":  fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"password":   acctest.RandString(10),
		"gmailUser":  gmailUser,
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailSendAsAlias_basic(data),
			},
			{
				ResourceName:      "googleworkspace_gmail_send_as_alias.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				Config: testAccGmailSendAsAlias_full(data),
			},
			{
				ResourceName:      "googleworkspace_gmail_send_as_alias.test",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccResourceGmailSendAsAlias_defaultCannotBeSetToFalseDirectly(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")

	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	data := map[string]interface{}{
		"domainName": domainName,
		"userEmail":  fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"password":   acctest.RandString(10),
		"gmailUser":  gmailUser,
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailSendAsAlias_withDefault(data),
			},
			{
				Config:      testAccGmailSendAsAlias_basic(data),
				ExpectError: regexp.MustCompile("isDefault cannot be toggled to false"),
			},
		},
	})
}

func TestAccResourceGmailSendAsAlias_switchingDefaultCausesDiff(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	domainName := os.Getenv("GOOGLEWORKSPACE_DOMAIN")

	if domainName == "" {
		t.Skip("GOOGLEWORKSPACE_DOMAIN needs to be set to run this test")
	}

	data := map[string]interface{}{
		"domainName": domainName,
		"userEmail":  fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"password":   acctest.RandString(10),
		"userEmail2": fmt.Sprintf("tf-test-%s", acctest.RandString(10)),
		"gmailUser":  gmailUser,
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailSendAsAlias_withDefaultUser1(data),
			},
			{
				Config:             testAccGmailSendAsAlias_withDefaultUser2(data),
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func testAccGmailSendAsAlias_basic(data map[string]interface{}) string {
	return Nprintf(`
data "googleworkspace_user" "test" {
  primary_email = "%{gmailUser}"
}

resource "googleworkspace_user" "alias" {
  primary_email = "%{userEmail}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Scott"
    given_name = "Michael"
  }
}

resource "googleworkspace_gmail_send_as_alias" "test" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias.primary_email
}
`, data)
}

func testAccGmailSendAsAlias_full(data map[string]interface{}) string {
	return Nprintf(`
data "googleworkspace_user" "test" {
  primary_email = "%{gmailUser}"
}

resource "googleworkspace_user" "alias" {
  primary_email = "%{userEmail}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Scott"
    given_name = "Michael"
  }
}

resource "googleworkspace_gmail_send_as_alias" "test" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias.primary_email
  display_name = "test"
  reply_to_address = "test@test.com"
  signature = "<b>yours truly</b>"
  treat_as_alias = false
}
`, data)
}

func testAccGmailSendAsAlias_withDefault(data map[string]interface{}) string {
	return Nprintf(`
data "googleworkspace_user" "test" {
  primary_email = "%{gmailUser}"
}

resource "googleworkspace_user" "alias" {
  primary_email = "%{userEmail}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Scott"
    given_name = "Michael"
  }
}

resource "googleworkspace_gmail_send_as_alias" "test" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias.primary_email
  is_default = true
}
`, data)
}

func testAccGmailSendAsAlias_withDefaultUser1(data map[string]interface{}) string {
	return Nprintf(`
data "googleworkspace_user" "test" {
  primary_email = "%{gmailUser}"
}

resource "googleworkspace_user" "alias" {
  primary_email = "%{userEmail}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Scott"
    given_name = "Michael"
  }
}

resource "googleworkspace_user" "alias2" {
  primary_email = "%{userEmail2}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Schrute"
    given_name = "Dwight"
  }
}

resource "googleworkspace_gmail_send_as_alias" "test" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias.primary_email
  is_default = true
}

resource "googleworkspace_gmail_send_as_alias" "test2" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias2.primary_email
}
`, data)
}

func testAccGmailSendAsAlias_withDefaultUser2(data map[string]interface{}) string {
	return Nprintf(`
data "googleworkspace_user" "test" {
  primary_email = "%{gmailUser}"
}

resource "googleworkspace_user" "alias" {
  primary_email = "%{userEmail}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Scott"
    given_name = "Michael"
  }
}

resource "googleworkspace_user" "alias2" {
  primary_email = "%{userEmail2}@%{domainName}"
  password = "%{password}"

  name {
    family_name = "Schrute"
    given_name = "Dwight"
  }
}

resource "googleworkspace_gmail_send_as_alias" "test" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias.primary_email
  is_default = true
}

resource "googleworkspace_gmail_send_as_alias" "test2" {
  primary_email = data.googleworkspace_user.test.primary_email
  send_as_email = googleworkspace_user.alias2.primary_email
  is_default = true
}
`, data)
}

func TestResourceGmailSendAsAliasImport(t *testing.T) {
	cases := map[string]struct {
		id          string
		wantPrimary string
		wantSendAs  string
		wantErr     bool
	}{
		"plain addresses": {
			id:          "user@example.com:alias@example.com",
			wantPrimary: "user@example.com",
			wantSendAs:  "alias@example.com",
		},
		"plus addressing and subdomain": {
			id:          "first.last@example.com:alias+tag@mail.example.co.uk",
			wantPrimary: "first.last@example.com",
			wantSendAs:  "alias+tag@mail.example.co.uk",
		},
		"colon in quoted local part of send-as address": {
			id:          `user@example.com:"a:b"@example.com`,
			wantPrimary: "user@example.com",
			wantSendAs:  `"a:b"@example.com`,
		},
		"colon in IPv6 literal of send-as address": {
			id:          "user@example.com:alias@[IPv6:2001:db8::1]",
			wantPrimary: "user@example.com",
			wantSendAs:  "alias@[IPv6:2001:db8::1]",
		},
		"no separator": {
			id:      "user@example.com",
			wantErr: true,
		},
		"empty primary email": {
			id:      ":alias@example.com",
			wantErr: true,
		},
		"empty send-as email": {
			id:      "user@example.com:",
			wantErr: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := schema.TestResourceDataRaw(t, resourceGmailSendAsAlias().Schema, map[string]interface{}{})
			d.SetId(tc.id)

			got, err := resourceGmailSendAsAliasImport(context.Background(), d, nil)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for ID %q, got none", tc.id)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for ID %q: %s", tc.id, err)
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 resource, got %d", len(got))
			}
			if v := got[0].Get("primary_email").(string); v != tc.wantPrimary {
				t.Errorf("primary_email: expected %q, got %q", tc.wantPrimary, v)
			}
			if v := got[0].Get("send_as_email").(string); v != tc.wantSendAs {
				t.Errorf("send_as_email: expected %q, got %q", tc.wantSendAs, v)
			}
			if got[0].Id() != tc.id {
				t.Errorf("ID: expected %q, got %q", tc.id, got[0].Id())
			}
		})
	}
}
