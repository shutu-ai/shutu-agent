package extensionhost

import (
	"testing"

	"github.com/shutu-ai/shutu-agent/sdk/extension"
)

func boolPtr(value bool) *bool { return &value }

func TestWebContributionsNavigationMetadataAndOrder(t *testing.T) {
	webItem := func(id, title, icon, group string, order int, enabled bool, ready bool) *managedExtension {
		item := &managedExtension{
			manifest: extension.Manifest{
				ID: id,
				Web: extension.WebContribution{
					Enabled: true, Route: "/extensions/" + id + "/", Title: title, Icon: icon,
					NavigationEnabled: boolPtr(enabled), NavigationGroup: group, Order: order,
				},
			},
			initialized: extension.InitializeResult{Capabilities: extension.Capabilities{Web: true}},
			webURL:      "http://127.0.0.1:1/",
		}
		item.ready.Store(ready)
		return item
	}
	h := &Host{items: []*managedExtension{
		webItem("zeta", "Zeta", "", "Extensions", 20, true, true),
		webItem("disabled", "Disabled", "", "Extensions", 1, false, true),
		webItem("alpha", "Alpha", "📊", "Data", 20, true, true),
		webItem("unhealthy", "Unhealthy", "", "Extensions", 10, true, false),
	}}
	got := h.WebContributions()
	if len(got) != 3 {
		t.Fatalf("web contributions = %#v", got)
	}
	if got[0].ExtensionID != "alpha" || got[1].ExtensionID != "unhealthy" || got[2].ExtensionID != "zeta" {
		t.Fatalf("navigation order = %#v", got)
	}
	if got[1].Ready || got[0].NavigationGroup != "Data" || !got[2].NavigationEnabled || got[2].NavigationGroup != "Extensions" {
		t.Fatalf("navigation metadata = %#v", got)
	}
}
