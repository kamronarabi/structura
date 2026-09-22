package resolve

import (
	"fmt"
	"sort"

	"github.com/kamronarabi/structura/pkg/schema"
)

// mergeDirectoryDuplicates folds several files describing one codebase into
// one component.
//
// A go.mod and a Dockerfile in ./checkout are one thing: the module and the
// image built from it. Left alone they are two boxes, and worse than that,
// they break the join that matters -- a Compose build context pointing at
// ./checkout requires exactly one component there, so a repository that
// gained a Dockerfile would lose the link between its container and its code.
//
// The rule is the directory, which is not an inference. Two files in one
// directory describing code describe the same code; that is what a directory
// is. Nothing here matches on names, so none of the caution the basename join
// needs applies.
//
// Which node survives is decided by whether it has a name of its own. A
// go.mod declares a module; a Dockerfile can only be named after the
// directory it sits in, and records that in nameFrom. A declared name beats a
// derived one, because the derived one is a placeholder that exists so the box
// has a label.
func (r *resolver) mergeDirectoryDuplicates() {
	byDir := map[string][]string{}
	for _, id := range r.index.AllIdentities() {
		// The same test codeByDirectoryBase uses: a codebase has a directory
		// and no image. A deployment is never folded away here.
		if id.Directory == "" || len(id.Images) > 0 {
			continue
		}
		byDir[id.Directory] = append(byDir[id.Directory], id.NodeID)
	}

	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		ids := byDir[dir]
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)

		var declared, derived []string
		for _, id := range ids {
			if n, ok := r.byID[id]; ok && n.Attrs["nameFrom"] != nil {
				derived = append(derived, id)
				continue
			}
			declared = append(declared, id)
		}

		// Nothing derived means every file here named itself, which is not
		// this fold's business: two manifests declaring two modules in one
		// directory may well be two things, and were drawn as two before.
		if len(derived) == 0 {
			continue
		}

		switch len(declared) {
		case 0:
			// Only derived names, so they are all named after the directory
			// and there is nothing to choose between them. The first in ID
			// order survives, which makes the outcome the same on every run.
			r.foldInto(derived[0], derived[1:])
		case 1:
			r.foldInto(declared[0], derived)
		default:
			// A directory declaring two modules and also holding a Dockerfile
			// does not say which module the image is built from. Guessing
			// would move one codebase's dependencies onto the other's box.
			r.diags = append(r.diags, schema.Diagnostic{
				Severity: schema.SeverityInfo,
				Code:     "directory_declares_several_components",
				Path:     dir,
				Message: fmt.Sprintf(
					"%s holds %d components with names of their own and %d named after the "+
						"directory, so it is not clear which the latter belong to; they are "+
						"drawn separately",
					dir, len(declared), len(derived)),
			})
		}
	}
}

// foldInto records that several nodes are the survivor, and moves what only
// they knew onto it.
func (r *resolver) foldInto(survivorID string, absorbed []string) {
	survivor, ok := r.byID[survivorID]
	if !ok {
		return
	}
	for _, id := range absorbed {
		node, ok := r.byID[id]
		if !ok || id == survivorID {
			continue
		}
		contributeInto(survivor, node)
		r.merges[id] = survivorID
	}
}

// contributedAttrs are the attributes a node may hand to the component it
// merges into. They are listed rather than copied wholesale because most
// attributes describe the file that produced them, and nameFrom in particular
// must not travel: a component that absorbed a directory-named node still has
// whatever name it declared.
var contributedAttrs = []string{
	"module", "package", "manifest", "directory", "dependencyCount",
	"goVersion", "nodeVersion", "pythonVersion", "version",
	"dockerfile", "baseImage", "buildStages", "ports",
}

// contributeInto fills gaps in dst from src. Nothing already known is
// overwritten: the surviving node was chosen because it is the better
// description, so a conflict is resolved in its favour.
func contributeInto(dst, src *schema.Node) {
	if src.Tech != nil {
		if dst.Tech == nil {
			dst.Tech = &schema.Tech{}
		}
		if dst.Tech.Language == "" {
			dst.Tech.Language = src.Tech.Language
		}
		if dst.Tech.Runtime == "" {
			dst.Tech.Runtime = src.Tech.Runtime
		}
		if dst.Tech.Framework == "" {
			dst.Tech.Framework = src.Tech.Framework
		}
	}
	for _, key := range contributedAttrs {
		v, ok := src.Attrs[key]
		if !ok {
			continue
		}
		if dst.Attrs == nil {
			dst.Attrs = schema.Attrs{}
		}
		if _, present := dst.Attrs[key]; !present {
			dst.Attrs[key] = v
		}
	}
	dst.Sources = append(dst.Sources, src.Sources...)
}
