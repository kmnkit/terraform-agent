package review

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/terraform-agent/internal/gitlab"
	"github.com/terraform-agent/internal/hclscan"
	"github.com/terraform-agent/internal/input"
	"github.com/terraform-agent/internal/llm"
	"github.com/terraform-agent/internal/model"
	"github.com/terraform-agent/internal/publish"
	"github.com/terraform-agent/internal/render"
	"github.com/terraform-agent/internal/staticcheck"
	"github.com/terraform-agent/internal/tfdocs"
)

// codeTarget is the note slot of the code review: kind "code", fixed scope "-".
var codeTarget = publish.Target{Kind: "code", Scope: "-"}

// conventionalFiles may hold required_providers next to a changed file.
var conventionalFiles = []string{"versions.tf", "providers.tf", "terraform.tf", "main.tf"}

// basisRank orders bases from weakest to strongest evidence.
var basisRank = map[tfdocs.Basis]int{tfdocs.BasisNone: 0, tfdocs.BasisConstraint: 1, tfdocs.BasisLock: 2}

// diffSet holds the MR diffs: raw (uncapped, for scanning) and the capped
// per-file .tf excerpts that may be handed to the LLM.
type diffSet struct {
	raw       []gitlab.FileDiff
	excerpts  map[string]string
	truncated bool
	collapsed int
}

// reviewItem is one changed resource with its review address, which is unique
// across the MR (see reviewAddresses). Docs, DocRefs and LLM knownAddrs are
// all keyed by it, so equal type.name pairs in different files never mix.
type reviewItem struct {
	rc   hclscan.ResourceChange
	addr string
}

// RunCode reviews the MR's Terraform changes and upserts the code review note.
// An MR without .tf changes gets no note and no LLM call. If nothing was
// detected only because GitLab collapsed .tf diffs, a truncation-only note is
// posted without an LLM call.
func RunCode(ctx context.Context, d Deps, in input.Input) error {
	ds, err := fetchDiffs(ctx, d, in.MRIID)
	if err != nil {
		return err
	}
	head := headReader{gl: d.GitLab, sha: in.SHA}
	changes, err := hclscan.ChangedResources(ctx, ds.raw, head)
	if err != nil {
		return fmt.Errorf("review: scan changes: %w", err)
	}
	if len(changes.Resources) == 0 && len(changes.Modules) == 0 {
		return handleNoChanges(ctx, d, in, ds)
	}
	d.Log.Info("review: changes detected", "resources", len(changes.Resources), "modules", len(changes.Modules))
	view, rin, docs, err := prepare(ctx, d, in, head, ds, changes)
	if err != nil {
		return err
	}
	findings, llmErr := d.Narrator.ReviewFindings(ctx, rin)
	if llmErr != nil {
		return publishAnalysisFailed(ctx, d, in, view, llmErr)
	}
	view.Findings = withDocRefs(findings, docs)
	return publishBody(ctx, d, in, view)
}

// handleNoChanges posts nothing when no .tf diff was collapsed; otherwise it
// posts the truncation notice with static checks and no findings.
func handleNoChanges(ctx context.Context, d Deps, in input.Input, ds diffSet) error {
	if ds.collapsed == 0 {
		d.Log.Info("review: no terraform resource or module changes; nothing to post")
		return nil
	}
	d.Log.Info("review: only collapsed .tf diffs; posting truncation notice", "collapsedTfDiffs", ds.collapsed)
	checks, err := staticChecks(ctx, d, in.PipelineID)
	if err != nil {
		return err
	}
	view := render.CodeReviewView{SHA: in.SHA, Checks: checks, Truncated: true, VersionBasis: string(tfdocs.BasisNone)}
	return publishBody(ctx, d, in, view)
}

// prepare reads static checks first (no MCP traffic is spent if that fails),
// then docs, and builds the view and LLM input.
func prepare(ctx context.Context, d Deps, in input.Input, head hclscan.FileReader, ds diffSet,
	changes hclscan.Changes) (render.CodeReviewView, llm.ReviewInput, map[string]tfdocs.Excerpt, error) {
	checks, err := staticChecks(ctx, d, in.PipelineID)
	if err != nil {
		return render.CodeReviewView{}, llm.ReviewInput{}, nil, err
	}
	decls, err := providerDecls(ctx, head, ds.raw)
	if err != nil {
		return render.CodeReviewView{}, llm.ReviewInput{}, nil, err
	}
	items := reviewItems(changes.Resources)
	docs, basis := lookupDocs(ctx, d, head, items, decls)
	view := render.CodeReviewView{SHA: in.SHA, Checks: checks, Truncated: ds.truncated, VersionBasis: string(basis)}
	return view, buildInput(items, changes.Modules, ds, docs, checks), docs, nil
}

func fetchDiffs(ctx context.Context, d Deps, iid int) (diffSet, error) {
	raw, err := d.GitLab.MergeRequestDiffs(ctx, iid)
	if err != nil {
		return diffSet{}, fmt.Errorf("review: fetch merge request diffs: %w", err)
	}
	return capDiffs(raw, d.Config.Limits.DiffBytes), nil
}

// capDiffs spends the byte budget over .tf diffs only, once per file. Scanning
// still uses the raw diffs: a diff cut mid-hunk would hide resources. Any .tf
// file with an empty diff is treated as collapsed by GitLab, so it counts as
// truncated.
func capDiffs(raw []gitlab.FileDiff, limit int) diffSet {
	out := diffSet{raw: raw, excerpts: map[string]string{}}
	budget := limit
	for _, fd := range raw {
		p := diffPath(fd)
		if !strings.HasSuffix(p, ".tf") {
			continue
		}
		if fd.Diff == "" {
			out.truncated = true
			out.collapsed++
			continue
		}
		text, cut := truncateUTF8(fd.Diff, budget)
		budget -= len(text)
		out.excerpts[p] = text
		out.truncated = out.truncated || cut
	}
	return out
}

// diffPath names a diff the way hclscan does: deleted files by their old path.
func diffPath(fd gitlab.FileDiff) string {
	if fd.DeletedFile {
		return fd.OldPath
	}
	return fd.NewPath
}

// truncateUTF8 cuts s to at most maxBytes without splitting a rune.
func truncateUTF8(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	cut := max(maxBytes, 0)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// headReader reads files at the MR head. A GitLab 404 also matches
// fs.ErrNotExist, which hclscan and tfdocs use to skip missing files.
type headReader struct {
	gl  GitLabReader
	sha string
}

func (h headReader) ReadFile(ctx context.Context, p string) ([]byte, error) {
	b, err := h.gl.FileAtRef(ctx, p, h.sha)
	if errors.Is(err, gitlab.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", fs.ErrNotExist, err)
	}
	return b, err
}

// providerDecls parses required_providers per directory (a Terraform root or
// module): the changed .tf files there plus the conventional files beside
// them. Missing files are skipped. The result is keyed by directory.
func providerDecls(ctx context.Context, head hclscan.FileReader,
	diffs []gitlab.FileDiff) (map[string]hclscan.ProviderDecls, error) {
	out := map[string]hclscan.ProviderDecls{}
	for dir, paths := range declPathsByDir(diffs) {
		files, err := readExisting(ctx, head, paths)
		if err != nil {
			return nil, err
		}
		decls, err := hclscan.ParseProviderDecls(files)
		if err != nil {
			return nil, fmt.Errorf("review: provider declarations in %s: %w", dir, err)
		}
		out[dir] = decls
	}
	return out, nil
}

func declPathsByDir(diffs []gitlab.FileDiff) map[string][]string {
	out := map[string][]string{}
	for _, fd := range diffs {
		if fd.DeletedFile || !strings.HasSuffix(fd.NewPath, ".tf") {
			continue
		}
		dir := path.Dir(fd.NewPath)
		if _, ok := out[dir]; !ok {
			for _, name := range conventionalFiles {
				out[dir] = append(out[dir], path.Join(dir, name))
			}
		}
		if !slices.Contains(out[dir], fd.NewPath) {
			out[dir] = append(out[dir], fd.NewPath)
		}
	}
	return out
}

// readExisting reads paths at head, skipping files that do not exist.
func readExisting(ctx context.Context, head hclscan.FileReader, paths []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, p := range paths {
		src, err := head.ReadFile(ctx, p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("review: read %s: %w", p, err)
		}
		files[p] = src
	}
	return files, nil
}

// lookupDocs fetches docs for every non-removed resource, keyed by review
// address, using the provider declarations of the resource's own directory.
// It returns the weakest basis among them (none when there are none).
func lookupDocs(ctx context.Context, d Deps, head hclscan.FileReader, items []reviewItem,
	decls map[string]hclscan.ProviderDecls) (map[string]tfdocs.Excerpt, tfdocs.Basis) {
	docs := map[string]tfdocs.Excerpt{}
	var worst tfdocs.Basis
	for _, it := range items {
		if it.rc.Removed {
			continue
		}
		basis, ex := resourceDoc(ctx, d, head, it, decls[path.Dir(it.rc.File)])
		if basis != tfdocs.BasisNone {
			docs[it.addr] = ex
		}
		if worst == "" || basisRank[basis] < basisRank[worst] {
			worst = basis
		}
	}
	if worst == "" {
		return docs, tfdocs.BasisNone
	}
	return docs, worst
}

// resourceDoc resolves the provider version and looks up docs for one
// resource. Any failure is logged (address and error only) and yields none,
// so the review continues without documentation for that resource.
func resourceDoc(ctx context.Context, d Deps, head hclscan.FileReader, it reviewItem,
	decls hclscan.ProviderDecls) (tfdocs.Basis, tfdocs.Excerpt) {
	rc := it.rc
	ns, name := hclscan.ProviderFor(rc, rc.ProviderMeta, decls)
	if !hclscan.IsPublicRegistry(ns) {
		return tfdocs.BasisNone, tfdocs.Excerpt{}
	}
	constraint := decls.Constraints[hclscan.LocalName(rc, rc.ProviderMeta)]
	ref, basis, err := tfdocs.ResolveVersion(ctx, d.MCP, rc.File, head, ns, name, constraint)
	if err != nil {
		d.Log.Warn("review: provider version not resolved; reviewing without docs", "resource", it.addr, "err", err)
		return tfdocs.BasisNone, tfdocs.Excerpt{}
	}
	if ref.Version == "" {
		return tfdocs.BasisNone, tfdocs.Excerpt{}
	}
	ex, err := d.Docs.Lookup(ctx, d.MCP, ref, rc.Type, rc.ChangedAttrs, d.Config.Limits.DocsBytes)
	if err != nil {
		d.Log.Warn("review: doc lookup failed; reviewing without docs", "resource", it.addr, "err", err)
		return tfdocs.BasisNone, tfdocs.Excerpt{}
	}
	return basis, ex
}

func reviewItems(rcs []hclscan.ResourceChange) []reviewItem {
	addrs := reviewAddresses(rcs)
	items := make([]reviewItem, len(rcs))
	for i, rc := range rcs {
		items[i] = reviewItem{rc: rc, addr: addrs[i]}
	}
	return items
}

// reviewAddresses returns an MR-unique address per resource (parallel to
// rcs): plain type.name when only one file has it, else "<dir>/type.name"
// when those files are in distinct directories, else "<file>:type.name".
func reviewAddresses(rcs []hclscan.ResourceChange) []string {
	filesByAddr := map[string]map[string]bool{}
	for _, rc := range rcs {
		a := address(rc)
		if filesByAddr[a] == nil {
			filesByAddr[a] = map[string]bool{}
		}
		filesByAddr[a][rc.File] = true
	}
	out := make([]string, len(rcs))
	for i, rc := range rcs {
		out[i] = uniqueAddress(rc, filesByAddr[address(rc)])
	}
	return out
}

func uniqueAddress(rc hclscan.ResourceChange, files map[string]bool) string {
	a := address(rc)
	if len(files) <= 1 {
		return a
	}
	dirs := map[string]bool{}
	for f := range files {
		dirs[path.Dir(f)] = true
	}
	if len(dirs) == len(files) {
		return path.Dir(rc.File) + "/" + a
	}
	return rc.File + ":" + a
}

func address(rc hclscan.ResourceChange) string {
	return rc.Type + "." + rc.Name
}

// staticChecks reads the static-check artifact of this pipeline. A missing job
// or artifact, or an unreadable artifact, means no checks.
func staticChecks(ctx context.Context, d Deps, pipelineID int) ([]model.CheckResult, error) {
	jobs, err := d.GitLab.PipelineJobs(ctx, int64(pipelineID))
	if err != nil {
		return nil, fmt.Errorf("review: list pipeline jobs: %w", err)
	}
	jobID, ok := staticJobID(jobs)
	if !ok {
		d.Log.Info("review: static check job not found")
		return nil, nil
	}
	raw, err := d.GitLab.JobArtifact(ctx, jobID, staticcheck.ArtifactName)
	if errors.Is(err, gitlab.ErrNotFound) {
		d.Log.Info("review: static check artifact not found", "job", jobID)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("review: fetch static check artifact: %w", err)
	}
	checks, err := staticcheck.Parse(raw)
	if err != nil {
		d.Log.Warn("review: static check artifact unreadable; ignoring", "job", jobID, "err", err)
		return nil, nil
	}
	return checks, nil
}

// staticJobID returns the newest (highest ID) job named staticcheck.JobName.
func staticJobID(jobs []gitlab.Job) (int64, bool) {
	var id int64
	for _, j := range jobs {
		if j.Name == staticcheck.JobName && j.ID > id {
			id = j.ID
		}
	}
	return id, id != 0
}

// buildInput uses the review addresses, so the narrator's knownAddrs (built
// from Resources) and the DocRef fill share one key space.
func buildInput(items []reviewItem, modules []hclscan.ModuleCall, ds diffSet, docs map[string]tfdocs.Excerpt,
	checks []model.CheckResult) llm.ReviewInput {
	res := make([]llm.ReviewResource, 0, len(items))
	for _, it := range items {
		ex := docs[it.addr]
		res = append(res, llm.ReviewResource{
			Address: it.addr, File: it.rc.File, ChangedAttrs: it.rc.ChangedAttrs, Removed: it.rc.Removed,
			DiffExcerpt: ds.excerpts[it.rc.File], DocExcerpt: ex.Text, DocTruncated: ex.Truncated,
		})
	}
	mods := make([]llm.ModuleContext, 0, len(modules))
	for _, m := range modules {
		mods = append(mods, llm.ModuleContext{Name: m.Name, Source: m.Source, Version: m.Version})
	}
	return llm.ReviewInput{Resources: res, Modules: mods, Checks: checks, DiffTruncated: ds.truncated}
}

// withDocRefs returns copies of findings with DocRef set from the doc URL
// known for each finding's resource.
func withDocRefs(findings []model.Finding, docs map[string]tfdocs.Excerpt) []model.Finding {
	out := make([]model.Finding, len(findings))
	for i, f := range findings {
		f.DocRef = docs[f.Resource].DocURL
		out[i] = f
	}
	return out
}

// publishAnalysisFailed posts the analysis-failed variant, then returns the
// LLM error (joined with any publish error).
func publishAnalysisFailed(ctx context.Context, d Deps, in input.Input, view render.CodeReviewView, llmErr error) error {
	view.AnalysisFailed = true
	wrapped := fmt.Errorf("review: review findings: %w", llmErr)
	if err := publishBody(ctx, d, in, view); err != nil {
		return errors.Join(wrapped, err)
	}
	return wrapped
}

// publishBody upserts the rendered note. A stale head is not an error: a
// newer run will post for the new head.
func publishBody(ctx context.Context, d Deps, in input.Input, view render.CodeReviewView) error {
	err := publish.Upsert(ctx, d.Notes, in.MRIID, in.SHA, codeTarget, render.CodeReview(view))
	if errors.Is(err, publish.ErrStale) {
		d.Log.Info("review: merge request head moved; skipping publish")
		return nil
	}
	if err != nil {
		return fmt.Errorf("review: publish code review: %w", err)
	}
	return nil
}
