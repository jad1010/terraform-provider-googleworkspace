// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
)

// The Gmail User Settings tests change the settings of a real mailbox. A
// destroy does not restore them, so the last step of each test leaves IMAP,
// POP and automatic forwarding disabled.

func TestAccResourceGmailUserSettings_imapPop(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	resourceName := "googleworkspace_gmail_user_settings.test"

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailUserSettings_imapPop(gmailUser, true, false, "trash", 1000, "allMail", "markRead"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "id", gmailUser),
					resource.TestCheckResourceAttr(resourceName, "imap.0.enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.auto_expunge", "false"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.expunge_behavior", "trash"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.max_folder_size", "1000"),
					resource.TestCheckResourceAttr(resourceName, "pop.0.access_window", "allMail"),
					resource.TestCheckResourceAttr(resourceName, "pop.0.disposition", "markRead"),
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.#", "0"),
				),
			},
			{
				ResourceName:      resourceName,
				ImportState:       true,
				ImportStateVerify: true,
				// The import reads all settings, but the configuration does
				// not manage automatic forwarding.
				ImportStateVerifyIgnore: []string{"auto_forwarding"},
			},
			{
				Config: testAccGmailUserSettings_imapPop(gmailUser, false, true, "archive", 0, "disabled", "leaveInInbox"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "imap.0.enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.auto_expunge", "true"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.expunge_behavior", "archive"),
					resource.TestCheckResourceAttr(resourceName, "imap.0.max_folder_size", "0"),
					resource.TestCheckResourceAttr(resourceName, "pop.0.access_window", "disabled"),
					resource.TestCheckResourceAttr(resourceName, "pop.0.disposition", "leaveInInbox"),
				),
			},
			{
				// Without the imap block, the IMAP settings are not managed.
				Config: testAccGmailUserSettings_popOnly(gmailUser),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "imap.#", "0"),
					resource.TestCheckResourceAttr(resourceName, "pop.0.access_window", "disabled"),
				),
			},
		},
	})
}

func TestAccResourceGmailUserSettings_autoForwarding(t *testing.T) {
	gmailUser := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_USER")

	if gmailUser == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_USER needs to be set to run this test")
	}

	// The forwarding address must be a forwarding address of the mailbox with
	// verification status accepted. Create it outside Terraform; addresses in
	// the same domain may not require verification.
	forwardingAddress := os.Getenv("GOOGLEWORKSPACE_TEST_GMAIL_FORWARDING_ADDRESS")

	if forwardingAddress == "" {
		t.Skip("GOOGLEWORKSPACE_TEST_GMAIL_FORWARDING_ADDRESS needs to be set to run this test")
	}

	resourceName := "googleworkspace_gmail_user_settings.test"

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: providerFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccGmailUserSettings_autoForwarding(gmailUser, true, forwardingAddress, "archive"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.0.enabled", "true"),
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.0.email_address", forwardingAddress),
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.0.disposition", "archive"),
				),
			},
			{
				ResourceName:            resourceName,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"imap", "pop"},
			},
			{
				Config: testAccGmailUserSettings_autoForwarding(gmailUser, false, forwardingAddress, "leaveInInbox"),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.0.enabled", "false"),
					resource.TestCheckResourceAttr(resourceName, "auto_forwarding.0.disposition", "leaveInInbox"),
				),
			},
		},
	})
}

func testAccGmailUserSettings_imapPop(gmailUser string, imapEnabled, autoExpunge bool, expungeBehavior string, maxFolderSize int, accessWindow, popDisposition string) string {
	return fmt.Sprintf(`
resource "googleworkspace_gmail_user_settings" "test" {
  user_email = "%s"

  imap {
    enabled          = %t
    auto_expunge     = %t
    expunge_behavior = "%s"
    max_folder_size  = %d
  }

  pop {
    access_window = "%s"
    disposition   = "%s"
  }
}
`, gmailUser, imapEnabled, autoExpunge, expungeBehavior, maxFolderSize, accessWindow, popDisposition)
}

func testAccGmailUserSettings_popOnly(gmailUser string) string {
	return fmt.Sprintf(`
resource "googleworkspace_gmail_user_settings" "test" {
  user_email = "%s"

  pop {
    access_window = "disabled"
  }
}
`, gmailUser)
}

func testAccGmailUserSettings_autoForwarding(gmailUser string, enabled bool, emailAddress, disposition string) string {
	return fmt.Sprintf(`
resource "googleworkspace_gmail_user_settings" "test" {
  user_email = "%s"

  auto_forwarding {
    enabled       = %t
    email_address = "%s"
    disposition   = "%s"
  }
}
`, gmailUser, enabled, emailAddress, disposition)
}
