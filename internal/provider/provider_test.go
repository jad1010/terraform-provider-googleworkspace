// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package googleworkspace

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

const testFakeCredentialsPath = "./test-data/fake-creds.json"

// providerFactories are used to instantiate a provider during acceptance testing.
// The factory function will be invoked for every Terraform CLI command executed
// to create a provider server to which the CLI can reattach.
var providerFactories = map[string]func() (*schema.Provider, error){
	"googleworkspace": func() (*schema.Provider, error) {
		return New("dev")(), nil
	},
}

var credsEnvVars = []string{
	"GOOGLEWORKSPACE_CREDENTIALS",
	"GOOGLEWORKSPACE_CLOUD_KEYFILE_JSON",
	"GOOGLEWORKSPACE_USE_DEFAULT_CREDENTIALS",
	"GOOGLE_CREDENTIALS",
}

func getTestCredsFromEnv() string {
	// Return empty string if GOOGLEWORKSPACE_USE_DEFAULT_CREDENTIALS is set to true.
	if os.Getenv("GOOGLEWORKSPACE_USE_DEFAULT_CREDENTIALS") == "true" {
		return ""
	}
	return multiEnvSearch(credsEnvVars)
}

func getTestCustomerFromEnv() string {
	return os.Getenv("GOOGLEWORKSPACE_CUSTOMER_ID")
}

func getTestImpersonatedUserFromEnv() string {
	return os.Getenv("GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL")
}

// googleworkspaceTestClient returns a common client
func googleworkspaceTestClient() (*apiClient, error) {
	creds := getTestCredsFromEnv()
	if creds == "" {
		return nil, fmt.Errorf("set credentials using any of these env variables %v", credsEnvVars)
	}

	customerId := getTestCustomerFromEnv()
	if customerId == "" {
		return nil, fmt.Errorf("set customer id with GOOGLEWORKSPACE_CUSTOMER_ID")
	}

	impersonatedUser := getTestImpersonatedUserFromEnv()
	if impersonatedUser == "" {
		return nil, fmt.Errorf("set customer id with GOOGLEWORKSPACE_IMPERSONATED_USER_EMAIL")
	}

	client := &apiClient{
		Credentials:           creds,
		Customer:              customerId,
		ImpersonatedUserEmail: impersonatedUser,
	}

	diags := client.loadAndValidate(context.Background())
	if diags.HasError() {
		log.Printf("[INFO][SWEEPER_LOG] error loading: %s", diags[0].Summary)
		return nil, fmt.Errorf("%s", diags[0].Summary)
	}

	return client, nil
}

func TestProvider(t *testing.T) {
	if err := New("dev")().InternalValidate(); err != nil {
		t.Fatalf("err: %s", err)
	}
}

// testAccPreCheck ensures at least one of the credentials env variables is set.
func testAccPreCheck(t *testing.T) {
	// You can add code here to run prior to any test case execution, for example assertions
	// about the appropriate environment variables being set are common to see in a pre-check
	// function.

	if v := multiEnvSearch(credsEnvVars); v == "" {
		t.Fatalf("One of %s must be set for acceptance tests", strings.Join(credsEnvVars, ", "))
	}
}

// testAccSkipUnlessWriteOnlySupported skips the test if the Terraform CLI that
// the test harness uses does not support write-only arguments. Terraform and
// OpenTofu support write-only arguments from version 1.11.
//
// terraform-plugin-sdk/v2 helper/resource has no TerraformVersionChecks, so this
// function selects the CLI binary with the same rules as resource.Test:
//   - TF_ACC_TERRAFORM_PATH, if the file exists. If it does not exist and
//     TF_ACC_TERRAFORM_VERSION is not set, the test fails.
//   - TF_ACC_TERRAFORM_VERSION, the version that the harness installs.
//   - terraform on PATH.
//
// If it finds none, the harness installs the latest Terraform, and the test runs.
func testAccSkipUnlessWriteOnlySupported(t *testing.T) {
	t.Helper()

	binPath := ""
	versionOutput := os.Getenv("TF_ACC_TERRAFORM_VERSION")

	if p := os.Getenv("TF_ACC_TERRAFORM_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			binPath = p
		} else if versionOutput == "" {
			t.Fatalf("TF_ACC_TERRAFORM_PATH is set to %q, but the file is not available: %s", p, err)
		}
	} else if versionOutput == "" {
		if p, err := exec.LookPath("terraform"); err == nil {
			binPath = p
		}
	}

	if binPath != "" {
		cmd := exec.Command(binPath, "version")
		// The harness runs the CLI in a temporary directory. Run it there too, so
		// that version manager shims (mise, asdf) select the same version.
		cmd.Dir = os.TempDir()
		cmd.Env = append(os.Environ(), "CHECKPOINT_DISABLE=1")

		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("cannot read the version of %s: %s", binPath, err)
		}

		versionOutput = string(out)
	}

	if versionOutput == "" {
		return
	}

	supported, err := writeOnlySupported(versionOutput)
	if err != nil {
		t.Fatal(err)
	}

	if !supported {
		t.Skipf("write-only arguments need Terraform or OpenTofu 1.11 or later, found %q",
			strings.SplitN(strings.TrimSpace(versionOutput), "\n", 2)[0])
	}
}

var cliVersionRegexp = regexp.MustCompile(`v?(\d+)\.(\d+)\.\d+`)

// writeOnlySupported returns true if the version string, or the first version
// in the output of "terraform version" or "tofu version", is 1.11 or later.
func writeOnlySupported(versionOutput string) (bool, error) {
	m := cliVersionRegexp.FindStringSubmatch(versionOutput)
	if m == nil {
		return false, fmt.Errorf("cannot find a version in %q", versionOutput)
	}

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])

	return major > 1 || (major == 1 && minor >= 11), nil
}

func multiEnvSearch(ks []string) string {
	for _, k := range ks {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// checkDiags will check to see if any of the diags have type error, and if so, they will return the first
// error. It will print warnings until it hits an error.
func checkDiags(diags diag.Diagnostics) error {
	if diags.HasError() {
		for _, d := range diags {
			if d.Severity == diag.Error {
				return fmt.Errorf("Error: %s (%s)", d.Summary, d.Detail)
			}

			fmt.Println("Warning: ", d.Detail)
		}
	}

	return nil
}
