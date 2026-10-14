package config

// applyImport merges the import overrides on top of the base stack loaded
// from the remote checkout and returns the merged stack.
//
// Merge semantics per the briefing:
//   - profiles, env_files, sops_files, vault_paths, hooks.*: concatenated
//     base-first and de-duplicated (first occurrence wins).
//   - services: if the import sets services, the import list replaces the
//     base list (subset filter); an empty or unset import list means "all"
//     and keeps the base list.
//   - name: the import name overrides the stack name when set; otherwise the
//     base name wins (the folder-name fallback is applied by the caller
//     after the merge).
//
// The import's sops_files are plain paths; they merge as SopsFile values
// without an age key file.
func applyImport(base Stack, imp Import) Stack {
	merged := base
	if imp.Name != "" {
		merged.Name = imp.Name
	}
	merged.Profiles = concatDedup(base.Profiles, imp.Profiles)
	merged.EnvFiles = concatDedup(base.EnvFiles, imp.EnvFiles)
	merged.VaultPaths = concatDedup(base.VaultPaths, imp.VaultPaths)
	merged.Hooks = Hooks{
		PreDeploy:  concatDedup(base.Hooks.PreDeploy, imp.Hooks.PreDeploy),
		PostDeploy: concatDedup(base.Hooks.PostDeploy, imp.Hooks.PostDeploy),
	}
	merged.SopsFiles = concatDedupSops(base.SopsFiles, importSopsFiles(imp))
	if len(imp.Services) > 0 {
		merged.Services = imp.Services
	}
	return merged
}

// concatDedup appends extra to base while dropping duplicates; the first
// occurrence wins, so base entries keep their order ahead of import entries.
// An empty result is normalized to nil.
func concatDedup(base, extra []string) []string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, list := range [2][]string{base, extra} {
		for _, v := range list {
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// concatDedupSops is concatDedup for SopsFile values, keyed by path. Base
// entries win, so a duplicate import path cannot override a base age key
// file.
func concatDedupSops(base, extra []SopsFile) []SopsFile {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]SopsFile, 0, len(base)+len(extra))
	for _, list := range [2][]SopsFile{base, extra} {
		for _, v := range list {
			if _, ok := seen[v.Path]; ok {
				continue
			}
			seen[v.Path] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

// importSopsFiles converts the import's plain sops file paths into SopsFile
// values.
func importSopsFiles(imp Import) []SopsFile {
	if len(imp.SopsFiles) == 0 {
		return nil
	}
	out := make([]SopsFile, 0, len(imp.SopsFiles))
	for _, p := range imp.SopsFiles {
		out = append(out, SopsFile{Path: p})
	}
	return out
}
