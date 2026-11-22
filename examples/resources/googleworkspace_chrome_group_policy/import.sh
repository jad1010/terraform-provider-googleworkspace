# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# The API supports group targets only for app, printer, and print server policies. These need additional target keys.

# <group_id>/<key>=<value>[+<key>=<value>...]/<schema names, separated by commas>
terraform import googleworkspace_chrome_group_policy.docs_offline 01abcde23fg4h5i/app_id=chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi/chrome.users.apps.InstallType

# A printer policy.
terraform import googleworkspace_chrome_group_policy.printer 01abcde23fg4h5i/printer_id=0gjdgxs1a2b3c4d/chrome.printers.AllowForUsers
