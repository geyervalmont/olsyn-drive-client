package platform

import "strings"

// OtherProvidersIn reads Windows' network provider order (a comma-separated
// list of service names) and names the other user-mode file systems in it
// that the client knows about. Only Dokany is recognised for now.
func OtherProvidersIn(order string) []string {
	var found []string
	for _, p := range strings.Split(order, ",") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "dokan") {
			found = append(found, "dokany")
			break
		}
	}
	return found
}
