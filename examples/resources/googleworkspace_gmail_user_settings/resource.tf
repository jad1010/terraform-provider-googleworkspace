# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# Disable IMAP, POP and automatic forwarding for one mailbox
# (CIS Google Workspace Foundations controls on POP/IMAP access and automatic forwarding).
# This is the setting of one mailbox, not the admin setting that controls
# whether users can forward mail. It does not stop forwarding by Gmail filters.
resource "googleworkspace_gmail_user_settings" "restricted" {
  user_email = "user@example.com"

  imap {
    enabled = false
  }

  pop {
    access_window = "disabled"
  }

  auto_forwarding {
    enabled = false
  }
}

# Manage only the IMAP settings of a mailbox. The POP and automatic
# forwarding settings are not managed.
resource "googleworkspace_gmail_user_settings" "imap" {
  user_email = "another.user@example.com"

  imap {
    enabled          = true
    auto_expunge     = false
    expunge_behavior = "trash"
    max_folder_size  = 1000
  }
}
