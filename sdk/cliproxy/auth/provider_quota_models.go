package auth

import "strings"

// ProviderQuotaModel resolves a registered route to its upstream identity and
// the keys used by selection and execution for model-scoped cooldowns.
func (m *Manager) ProviderQuotaModel(auth *Auth, route string) (string, []string) {
	if m == nil || auth == nil {
		return "", nil
	}
	requested := rewriteModelForAuth(route, auth)
	routing := m.loadAPIKeyModelRouting()
	alias := m.resolveExecutionAliasResultForRequestedWithRouting(routing, auth, requested)
	upstream := executionAliasPoolModel(auth, requested, alias)
	upstream = m.applyAPIKeyModelAliasWithRouting(routing, auth, upstream)
	if strings.TrimSpace(upstream) == "" {
		upstream = requested
	}
	keys := make([]string, 0, 2)
	for _, key := range []string{m.selectionModelKeyForAuth(auth, route), canonicalModelKey(m.stateModelForExecution(auth, route, upstream, false))} {
		if key != "" && (len(keys) == 0 || key != keys[0]) {
			keys = append(keys, key)
		}
	}
	return canonicalModelKey(upstream), keys
}
