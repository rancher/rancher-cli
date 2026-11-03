package kinds

// RoleTemplateLocked reports whether a decoded RoleTemplate is locked: its
// locked field is the JSON boolean true. Missing, false and any other type
// (a string "true", a number) count as unlocked.
func RoleTemplateLocked(obj map[string]any) bool {
	locked, _ := obj["locked"].(bool)
	return locked
}

// indexByName maps metadata.name to the decoded object, for the objects
// that have a name. A name that repeats keeps the first object.
func indexByName(objs []map[string]any) map[string]map[string]any {
	index := make(map[string]map[string]any, len(objs))
	for _, obj := range objs {
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if name == "" {
			continue
		}
		if _, ok := index[name]; !ok {
			index[name] = obj
		}
	}
	return index
}

// globalRoleInheritedTemplates returns the role template names in the
// global role's own inheritedClusterRoles. A missing field, a value that is
// not a list and a list entry that is not a string name nothing.
func globalRoleInheritedTemplates(role map[string]any) []string {
	list, _ := role["inheritedClusterRoles"].([]any)
	var names []string
	for _, v := range list {
		if name, ok := v.(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// anyGlobalRoleNamesTemplate reports whether any global role of roles names
// an inherited role template.
func anyGlobalRoleNamesTemplate(roles []map[string]any) bool {
	for _, role := range roles {
		if len(globalRoleInheritedTemplates(role)) > 0 {
			return true
		}
	}
	return false
}

// globalRoleInheritsLocked reports whether Rancher's webhook refuses a new
// binding to the global role: one of the role template names in its own
// inheritedClusterRoles is a locked role template of templates (indexed by
// name, as indexByName builds it), or is not in templates at all. The names
// are looked up as they are written, whatever the role template's context,
// and the role templates a template itself inherits are not followed.
func globalRoleInheritsLocked(role map[string]any, templates map[string]map[string]any) bool {
	for _, name := range globalRoleInheritedTemplates(role) {
		template, ok := templates[name]
		if !ok || RoleTemplateLocked(template) {
			return true
		}
	}
	return false
}

// BindableGlobalRoles returns the global roles of roles that Rancher accepts
// in a new binding, in their order. listTemplates reads the role templates;
// it is called once, and only when some global role names an inherited role
// template. When it fails, the roles that name no inherited role template
// are returned (they pass whatever the role templates are) together with the
// error.
func BindableGlobalRoles(roles []map[string]any, listTemplates func() ([]map[string]any, error)) ([]map[string]any, error) {
	if !anyGlobalRoleNamesTemplate(roles) {
		return roles, nil
	}
	templates, err := listTemplates()
	var out []map[string]any
	if err != nil {
		for _, role := range roles {
			if len(globalRoleInheritedTemplates(role)) == 0 {
				out = append(out, role)
			}
		}
		return out, err
	}
	index := indexByName(templates)
	for _, role := range roles {
		if !globalRoleInheritsLocked(role, index) {
			out = append(out, role)
		}
	}
	return out, nil
}
