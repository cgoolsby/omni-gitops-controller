# Prompt 13 — Remove Planning Docs Directory from Repo

## Context

The directory `omni-gitops-controller/` at the repo root contains 12 internal implementation planning markdown files:

```
omni-gitops-controller/
  00-create-github-repo.md
  01-scaffold-and-rename.md
  02-kubeconfig-namespace-flag.md
  03-scale-down.md
  04-install-manifests.md
  05-helm-chart.md
  06-readme.md
  07-examples.md
  08-github-ci.md
  09-blog-post.md
  10-config-drift-reboot.md
  PLAN.md
```

These are implementation scaffolding prompts used during the initial development of the controller. They are not user-facing content, not API docs, and not contributing guidelines. They add noise to the repository for external contributors and users who clone or browse the project.

The directory name (`omni-gitops-controller/`) is also confusing — it's the same as the repo name, making it look like a nested copy of the project.

---

## What to Do

Delete the entire `omni-gitops-controller/` directory from the repository:

```bash
git rm -r omni-gitops-controller/
git commit -m "chore: remove internal planning docs from repo"
```

If there is any content worth preserving for reference:
- `PLAN.md` describes the original architecture decisions — extract the relevant "Why Not CAPI?" reasoning (already in the README) and discard the rest.
- The individual prompt files describe what was built, not what users need.

Do not create a `docs/` directory or `PLANNING.md` as a replacement — the work is done and the code is the artifact.

---

## Verification

```bash
ls omni-gitops-controller/
# should return: ls: cannot access 'omni-gitops-controller/': No such file or directory

git log --oneline -3
# should show the removal commit
```

Confirm the repo root now contains only: `api/`, `charts/`, `config/`, `controllers/`, `examples/`, `.github/`, `Dockerfile`, `go.mod`, `go.sum`, `main.go`, `README.md`, `LICENSE`, `.gitignore`, `.golangci.yml`.
