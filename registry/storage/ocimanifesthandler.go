package storage

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"reflect"

	"github.com/distribution/distribution/v3"
	dcontext "github.com/distribution/distribution/v3/context"
	"github.com/distribution/distribution/v3/manifest/ocischema"
	"github.com/distribution/distribution/v3/registry/storage/driver"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

//ocischemaManifestHandler is a ManifestHandler that covers ocischema manifests.
type ocischemaManifestHandler struct {
	repository    distribution.Repository
	blobStore     distribution.BlobStore
	ctx           context.Context
	manifestURLs  manifestURLs
	storageDriver driver.StorageDriver
}

var _ ManifestHandler = &ocischemaManifestHandler{}

func (ms *ocischemaManifestHandler) Unmarshal(ctx context.Context, dgst digest.Digest, content []byte) (distribution.Manifest, error) {
	dcontext.GetLogger(ms.ctx).Debug("(*ocischemaManifestHandler).Unmarshal")

	m := &ocischema.DeserializedManifest{}
	if err := m.UnmarshalJSON(content); err != nil {
		return nil, err
	}

	return m, nil
}

func (ms *ocischemaManifestHandler) Put(ctx context.Context, manifest distribution.Manifest, skipDependencyVerification bool) (digest.Digest, error) {
	dcontext.GetLogger(ms.ctx).Debug("(*ocischemaManifestHandler).Put")

	m, ok := manifest.(*ocischema.DeserializedManifest)
	if !ok {
		return "", fmt.Errorf("non-ocischema manifest put to ocischemaManifestHandler: %T", manifest)
	}

	if err := ms.verifyManifest(ms.ctx, *m, skipDependencyVerification); err != nil {
		return "", err
	}

	mt, payload, err := m.Payload()
	if err != nil {
		return "", err
	}

	revision, err := ms.blobStore.Put(ctx, mt, payload)
	if err != nil {
		dcontext.GetLogger(ctx).Errorf("error putting payload into blobstore: %v", err)
		return "", err
	}

	if !reflect.DeepEqual(m.Reference, v1.Descriptor{}) {
		// add link file here if Reference field isn't empty
		err = ms.indexReferrers(ctx, m, revision.Digest)
		if err != nil {
			dcontext.GetLogger(ctx).Errorf("error indexing referrers: %v", err)
			return "", err
		}
	}

	return revision.Digest, nil
}

// verifyManifest ensures that the manifest content is valid from the
// perspective of the registry. As a policy, the registry only tries to store
// valid content, leaving trust policies of that content up to consumers.
func (ms *ocischemaManifestHandler) verifyManifest(ctx context.Context, mnfst ocischema.DeserializedManifest, skipDependencyVerification bool) error {
	var errs distribution.ErrManifestVerification

	if mnfst.Manifest.SchemaVersion != 2 {
		return fmt.Errorf("unrecognized manifest schema version %d", mnfst.Manifest.SchemaVersion)
	}

	if skipDependencyVerification {
		return nil
	}

	manifestService, err := ms.repository.Manifests(ctx)
	if err != nil {
		return err
	}

	blobsService := ms.repository.Blobs(ctx)

	for _, descriptor := range mnfst.References() {
		err := descriptor.Digest.Validate()
		if err != nil {
			errs = append(errs, err, distribution.ErrManifestBlobUnknown{Digest: descriptor.Digest})
			continue
		}

		switch descriptor.MediaType {
		case v1.MediaTypeImageLayer, v1.MediaTypeImageLayerGzip, v1.MediaTypeImageLayerNonDistributable, v1.MediaTypeImageLayerNonDistributableGzip:
			allow := ms.manifestURLs.allow
			deny := ms.manifestURLs.deny
			for _, u := range descriptor.URLs {
				var pu *url.URL
				pu, err = url.Parse(u)
				if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Fragment != "" || (allow != nil && !allow.MatchString(u)) || (deny != nil && deny.MatchString(u)) {
					err = errInvalidURL
					break
				}
			}
			if err == nil {
				// check the presence if it is normal layer or
				// there is no urls for non-distributable
				if len(descriptor.URLs) == 0 ||
					(descriptor.MediaType == v1.MediaTypeImageLayer || descriptor.MediaType == v1.MediaTypeImageLayerGzip) {

					_, err = blobsService.Stat(ctx, descriptor.Digest)
				}
			}

		case v1.MediaTypeImageManifest:
			var exists bool
			exists, err = manifestService.Exists(ctx, descriptor.Digest)
			if err != nil || !exists {
				err = distribution.ErrBlobUnknown // just coerce to unknown.
			}

			if err != nil {
				dcontext.GetLogger(ms.ctx).WithError(err).Debugf("failed to ensure exists of %v in manifest service", descriptor.Digest)
			}
			fallthrough // double check the blob store.
		default:
			// check the presence
			_, err = blobsService.Stat(ctx, descriptor.Digest)
		}

		if err != nil {
			if err != distribution.ErrBlobUnknown {
				errs = append(errs, err)
			}

			// On error here, we always append unknown blob errors.
			errs = append(errs, distribution.ErrManifestBlobUnknown{Digest: descriptor.Digest})
		}
	}

	if len(errs) != 0 {
		return errs
	}

	return nil
}

// indexReferrers indexes the subject of the given revision in its referrers index store.
func (ms *ocischemaManifestHandler) indexReferrers(ctx context.Context, dm *ocischema.DeserializedManifest, revision digest.Digest) error {
	subjectRevision := dm.Reference.Digest

	rootPath := path.Join(referrersLinkPath(ms.repository.Named().Name()), subjectRevision.Algorithm().String(), subjectRevision.Hex())
	referenceLinkPath := path.Join(rootPath, revision.Algorithm().String(), revision.Hex(), "link")
	if err := ms.storageDriver.PutContent(ctx, referenceLinkPath, []byte(revision.String())); err != nil {
		return err
	}

	return nil
}

func referrersLinkPath(name string) string {
	return path.Join("/docker/registry/", "v2", "repositories", name, "_refs", "subjects")
}
