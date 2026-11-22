# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

# <policy_schema>:<target_resource>:<key>=<value>[+<key>=<value>...]
terraform import googleworkspace_chrome_policy_group_priority_ordering.install_type chrome.users.apps.InstallType::app_id=chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi

# namespace=<policy_namespace>:<target_resource>:<key>=<value>[+<key>=<value>...]
terraform import googleworkspace_chrome_policy_group_priority_ordering.docs_offline namespace=chrome.users.apps::app_id=chrome:ghbmnnjooekpmoecnnnilnnbdlolhkhi
