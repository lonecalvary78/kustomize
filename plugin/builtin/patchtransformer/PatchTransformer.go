// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

//go:generate pluginator
package main

import (
	"fmt"
	"strings"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"sigs.k8s.io/kustomize/api/filters/patchjson6902"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/errors"
	"sigs.k8s.io/kustomize/kyaml/kio/kioutil"
	"sigs.k8s.io/yaml"
)

type plugin struct {
	smPatches   []*resource.Resource // strategic-merge patches
	jsonPatches jsonpatch.Patch      // json6902 patch
	// patchText is pure patch text created by Path or Patch
	patchText string
	// patchSource is patch source message
	patchSource string
	Path        string          `json:"path,omitempty"    yaml:"path,omitempty"`
	Patch       string          `json:"patch,omitempty"   yaml:"patch,omitempty"`
	Target      *types.Selector `json:"target,omitempty"  yaml:"target,omitempty"`
	Options     map[string]bool `json:"options,omitempty" yaml:"options,omitempty"`
}

var KustomizePlugin plugin //nolint:gochecknoglobals

func (p *plugin) Config(h *resmap.PluginHelpers, c []byte) error {
	if err := yaml.Unmarshal(c, p); err != nil {
		return err
	}

	p.Patch = strings.TrimSpace(p.Patch)
	switch {
	case p.Patch == "" && p.Path == "":
		return fmt.Errorf("must specify one of patch and path in\n%s", string(c))
	case p.Patch != "" && p.Path != "":
		return fmt.Errorf("patch and path can't be set at the same time\n%s", string(c))
	case p.Patch != "":
		p.patchText = p.Patch
		p.patchSource = fmt.Sprintf("[patch: %q]", p.patchText)
	case p.Path != "":
		loaded, err := h.Loader().Load(p.Path)
		if err != nil {
			return fmt.Errorf("failed to get the patch file from path(%s): %w", p.Path, err)
		}
		p.patchText = string(loaded)
		p.patchSource = fmt.Sprintf("[path: %q]", p.Path)
	}

	patchesSM, errSM := h.ResmapFactory().RF().SliceFromBytes([]byte(p.patchText))
	patchesJson, errJson := jsonPatchFromBytes([]byte(p.patchText))

	if ((errSM == nil && errJson == nil) ||
		(patchesSM != nil && patchesJson != nil)) &&
		(len(patchesSM) > 0 && len(patchesJson) > 0) {
		return fmt.Errorf(
			"illegally qualifies as both an SM and JSON patch: %s",
			p.patchSource)
	}
	if errSM != nil && errJson != nil {
		return fmt.Errorf(
			"unable to parse SM or JSON patch from %s", p.patchSource)
	}
	if errSM == nil {
		p.smPatches = patchesSM
		for _, loadedPatch := range p.smPatches {
			if p.Options["allowNameChange"] {
				loadedPatch.AllowNameChange()
			}
			if p.Options["allowKindChange"] {
				loadedPatch.AllowKindChange()
			}
		}
	} else {
		p.jsonPatches = patchesJson
	}
	return nil
}

func (p *plugin) Transform(m resmap.ResMap) error {
	if p.smPatches != nil {
		return p.transformStrategicMerge(m)
	}
	return p.transformJson6902(m)
}

// transformStrategicMerge applies each loaded strategic merge patch
// to the resource in the ResMap that matches the identifier of the patch.
// If only one patch is specified, the Target can be used instead.
func (p *plugin) transformStrategicMerge(m resmap.ResMap) error {
	if p.Target != nil {
		if len(p.smPatches) > 1 {
			// detail: https://github.com/kubernetes-sigs/kustomize/issues/5049#issuecomment-1440604403
			return fmt.Errorf("Multiple Strategic-Merge Patches in one `patches` entry is not allowed to set `patches.target` field: %s", p.patchSource)
		}

		// single patch
		patch := p.smPatches[0]
		selected, err := m.Select(*p.Target)
		if err != nil {
			return fmt.Errorf("unable to find patch target %q in `resources`: %w", p.Target, err)
		}
		return errors.Wrap(m.ApplySmPatch(resource.MakeIdSet(selected), patch))
	}

	for _, patch := range p.smPatches {
		target, err := m.GetById(patch.OrgId())
		if err != nil {
			return fmt.Errorf("no resource matches strategic merge patch %q: %w", patch.OrgId(), err)
		}
		if err := target.ApplySmPatch(patch); err != nil {
			return errors.Wrap(err)
		}
	}
	return nil
}

// transformJson6902 applies json6902 Patch to all the resources in the ResMap that match Target.
func (p *plugin) transformJson6902(m resmap.ResMap) error {
	if p.Target == nil {
		return fmt.Errorf("must specify a target for JSON patch %s", p.patchSource)
	}
	resources, err := m.Select(*p.Target)
	if err != nil {
		return err
	}
	for _, res := range resources {
		res.StorePreviousId()
		internalAnnotations := kioutil.GetInternalAnnotations(&res.RNode)
		err = res.ApplyFilter(patchjson6902.Filter{
			Patch: p.patchText,
		})
		if err != nil {
			return err
		}

		annotations := res.GetAnnotations()
		for key, value := range internalAnnotations {
			annotations[key] = value
		}
		err = res.SetAnnotations(annotations)
	}
	return nil
}

// jsonPatchFromBytes loads a Json 6902 patch from a bytes input
func jsonPatchFromBytes(in []byte) (jsonpatch.Patch, error) {
	ops := string(in)
	if ops == "" {
		return nil, fmt.Errorf("empty json patch operations")
	}

	if ops[0] != '[' {
		// TODO(5049):
		//   In the case of multiple yaml documents, return error instead of ignoring all but first.
		//   Details: https://github.com/kubernetes-sigs/kustomize/pull/5194#discussion_r1256686728
		jsonOps, err := yaml.YAMLToJSON(in)
		if err != nil {
			return nil, err
		}
		ops = string(jsonOps)
	}
	return jsonpatch.DecodePatch([]byte(ops))
}
