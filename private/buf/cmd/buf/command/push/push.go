// Copyright 2020-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package push

import (
	"context"
	"errors"
	"fmt"

	"github.com/bufbuild/buf/private/buf/bufcli"
	"github.com/bufbuild/buf/private/bufpkg/bufanalysis"
	"github.com/bufbuild/buf/private/bufpkg/bufmanifest"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule/bufmodulebuild"
	"github.com/bufbuild/buf/private/bufpkg/bufmodule/bufmoduleref"
	"github.com/bufbuild/buf/private/gen/proto/connect/buf/alpha/registry/v1alpha1/registryv1alpha1connect"
	registryv1alpha1 "github.com/bufbuild/buf/private/gen/proto/go/buf/alpha/registry/v1alpha1"
	"github.com/bufbuild/buf/private/pkg/app/appcmd"
	"github.com/bufbuild/buf/private/pkg/app/appflag"
	"github.com/bufbuild/buf/private/pkg/command"
	"github.com/bufbuild/buf/private/pkg/connectclient"
	"github.com/bufbuild/buf/private/pkg/manifest"
	"github.com/bufbuild/buf/private/pkg/stringutil"
	"github.com/bufbuild/connect-go"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	tagFlagName             = "tag"
	tagFlagShortName        = "t"
	draftFlagName           = "draft"
	errorFormatFlagName     = "error-format"
	disableSymlinksFlagName = "disable-symlinks"
	// deprecated
	trackFlagName = "track"
)

// NewCommand returns a new Command.
func NewCommand(
	name string,
	builder appflag.Builder,
) *appcmd.Command {
	flags := newFlags()
	return &appcmd.Command{
		Use:   name + " <source>",
		Short: "Push a module to a registry",
		Long:  bufcli.GetSourceLong(`the source to push`),
		Args:  cobra.MaximumNArgs(1),
		Run: builder.NewRunFunc(
			func(ctx context.Context, container appflag.Container) error {
				return run(ctx, container, flags)
			},
			bufcli.NewErrorInterceptor(),
		),
		BindFlags: flags.Bind,
	}
}

type flags struct {
	Tags            []string
	Draft           string
	ErrorFormat     string
	DisableSymlinks bool
	// Deprecated
	Tracks []string
	// special
	InputHashtag string
}

func newFlags() *flags {
	return &flags{}
}

func (f *flags) Bind(flagSet *pflag.FlagSet) {
	bufcli.BindInputHashtag(flagSet, &f.InputHashtag)
	bufcli.BindDisableSymlinks(flagSet, &f.DisableSymlinks, disableSymlinksFlagName)
	flagSet.StringSliceVarP(
		&f.Tags,
		tagFlagName,
		tagFlagShortName,
		nil,
		fmt.Sprintf(
			"Create a tag for the pushed commit. Multiple tags are created if specified multiple times. Cannot be used together with --%s",
			draftFlagName,
		),
	)
	flagSet.StringVar(
		&f.Draft,
		draftFlagName,
		"",
		fmt.Sprintf(
			"Make the pushed commit a draft with the specified name. Cannot be used together with --%s (-%s)",
			tagFlagName,
			tagFlagShortName,
		),
	)
	flagSet.StringVar(
		&f.ErrorFormat,
		errorFormatFlagName,
		"text",
		fmt.Sprintf(
			"The format for build errors printed to stderr. Must be one of %s",
			stringutil.SliceToString(bufanalysis.AllFormatStrings),
		),
	)
	flagSet.StringSliceVar(
		&f.Tracks,
		trackFlagName,
		nil,
		"Do not use. This flag never had any effect",
	)
	_ = flagSet.MarkHidden(trackFlagName)
}

func run(
	ctx context.Context,
	container appflag.Container,
	flags *flags,
) (retErr error) {
	if len(flags.Tracks) > 0 {
		return appcmd.NewInvalidArgumentErrorf("--%s has never had any effect, do not use.", trackFlagName)
	}
	if err := bufcli.ValidateErrorFormatFlag(flags.ErrorFormat, errorFormatFlagName); err != nil {
		return err
	}
	if len(flags.Tags) > 0 && flags.Draft != "" {
		return appcmd.NewInvalidArgumentErrorf("--%s (-%s) and --%s cannot be used together.", tagFlagName, tagFlagShortName, draftFlagName)
	}
	source, err := bufcli.GetInputValue(container, flags.InputHashtag, ".")
	if err != nil {
		return err
	}
	storageosProvider := bufcli.NewStorageosProvider(flags.DisableSymlinks)
	runner := command.NewRunner()
	// We are pushing to the BSR, this module has to be independently buildable
	// given the configuration it has without any enclosing workspace.
	sourceBucket, sourceConfig, err := bufcli.BucketAndConfigForSource(
		ctx,
		container.Logger(),
		container,
		storageosProvider,
		runner,
		source,
	)
	if err != nil {
		return err
	}
	moduleIdentity := sourceConfig.ModuleIdentity
	builtModule, err := bufmodulebuild.BuildForBucket(
		ctx,
		sourceBucket,
		sourceConfig.Build,
	)
	if err != nil {
		return err
	}
	modulePin, err := push(ctx, container, moduleIdentity, builtModule, flags)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			if _, err := container.Stderr().Write(
				[]byte("The latest commit has the same content; not creating a new commit.\n"),
			); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	if modulePin == nil {
		return errors.New("Missing local module pin in the registry's response.")
	}
	if _, err := container.Stdout().Write([]byte(modulePin.Commit + "\n")); err != nil {
		return err
	}
	return nil
}

func push(
	ctx context.Context,
	container appflag.Container,
	moduleIdentity bufmoduleref.ModuleIdentity,
	builtModule *bufmodulebuild.BuiltModule,
	flags *flags,
) (*registryv1alpha1.LocalModulePin, error) {
	clientConfig, err := bufcli.NewConnectClientConfig(container)
	if err != nil {
		return nil, err
	}
	service := connectclient.Make(clientConfig, moduleIdentity.Remote(), registryv1alpha1connect.NewPushServiceClient)
	// Check if tamper proofing env var is enabled
	tamperProofingEnabled, err := bufcli.IsBetaTamperProofingEnabled(container)
	if err != nil {
		return nil, err
	}
	if tamperProofingEnabled {
		m, blobSet, err := manifest.NewFromBucket(ctx, builtModule.Bucket)
		if err != nil {
			return nil, err
		}
		bucketManifest, blobs, err := bufmanifest.ToProtoManifestAndBlobs(ctx, m, blobSet)
		if err != nil {
			return nil, err
		}
		resp, err := service.PushManifestAndBlobs(
			ctx,
			connect.NewRequest(&registryv1alpha1.PushManifestAndBlobsRequest{
				Owner:      moduleIdentity.Owner(),
				Repository: moduleIdentity.Repository(),
				Manifest:   bucketManifest,
				Blobs:      blobs,
				Tags:       flags.Tags,
				DraftName:  flags.Draft,
			}),
		)
		if err != nil {
			return nil, err
		}
		return resp.Msg.LocalModulePin, nil
	}
	// Fall back to previous push call
	protoModule, err := bufmodule.ModuleToProtoModule(ctx, builtModule.Module)
	if err != nil {
		return nil, err
	}
	resp, err := service.Push(
		ctx,
		connect.NewRequest(&registryv1alpha1.PushRequest{
			Owner:      moduleIdentity.Owner(),
			Repository: moduleIdentity.Repository(),
			Module:     protoModule,
			Tags:       flags.Tags,
			DraftName:  flags.Draft,
		}),
	)
	if err != nil {
		return nil, err
	}
	return resp.Msg.LocalModulePin, nil
}
