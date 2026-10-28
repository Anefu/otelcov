package k8sattributes

// Image reference parsing as the processor does it, with the same library:
// collector-contrib 0.134's internal/common/docker/images.go (ParseImageName,
// CanonicalImageRef) and the k8sattributes processor's
// parseServiceVersionFromImage (internal/kube/client.go).
//
// Copyright The OpenTelemetry Authors, SPDX-License-Identifier: Apache-2.0.

import (
	"errors"
	"fmt"

	"github.com/distribution/reference"
)

type imageRef struct {
	repository string
	tag        string
	tagged     bool // false: no tag in the reference, so tag is "latest"
}

// parseImageName splits an image reference into repository and tag:
// example.com:5000/alpine/alpine:test is example.com:5000/alpine/alpine and
// test. Names aren't normalized: nginx stays nginx, not docker.io/library/nginx.
func parseImageName(image string) (imageRef, error) {
	ref, err := reference.Parse(image)
	if err != nil {
		return imageRef{}, err
	}
	named, ok := ref.(reference.Named)
	if !ok {
		return imageRef{}, errors.New("cannot retrieve image name")
	}
	img := imageRef{repository: named.Name(), tag: "latest"}
	if t, ok := named.(reference.Tagged); ok {
		img.tag, img.tagged = t.Tag(), true
	}
	return img, nil
}

// serviceVersionFromImage is service.version from an image: its tag, its
// digest, or both as tag@digest.
func serviceVersionFromImage(image string) (string, error) {
	ref, err := reference.Parse(image)
	if err != nil {
		return "", err
	}
	named, ok := ref.(reference.Named)
	if !ok {
		return "", errors.New("cannot retrieve image name")
	}
	var tag, digest string
	if t, ok := named.(reference.Tagged); ok {
		tag = t.Tag()
	}
	if d, ok := named.(reference.Digested); ok {
		digest = d.Digest().String()
	}
	switch {
	case digest != "" && tag != "":
		return fmt.Sprintf("%s@%s", tag, digest), nil
	case digest != "":
		return digest, nil
	case tag != "":
		return tag, nil
	}
	return "", errors.New("cannot retrieve image name")
}

// canonicalImageRef is a container status's imageID as a canonical
// reference (repository@digest), or an error if it isn't one.
func canonicalImageRef(image string) (string, error) {
	parsed, err := reference.ParseAnyReference(image)
	if err != nil {
		return "", err
	}
	if _, ok := parsed.(reference.Canonical); ok {
		return parsed.String(), nil
	}
	return "", errors.New("not canonical image reference")
}
