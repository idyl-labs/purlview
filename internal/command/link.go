// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package command

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/idyl-labs/purlview/internal/output"
	"github.com/idyl-labs/purlview/internal/share"
)

// newLink builds the link command: the link again, on request, for a share
// that list names only by id. Like share, it prints nothing else on stdout.
func newLink(p *output.Printer, d Deps) *cobra.Command {
	return &cobra.Command{
		Use:   "link <id>",
		Short: "Print a share's link",
		Long: `Print the link of one of your active shares, by its id from 'purlview list'.

The link is the only thing printed on standard output, so it can be piped
to the clipboard.`,
		Example: "  purlview link k7m2p4qx",
		Args: oneArg(
			misuse("link needs a share id").remedy("see ", bright("purlview list")),
			misuse("link takes one share at a time").remedy("see ", bright("purlview list")),
		),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := share.ParseRef(args[0])
			if err != nil {
				return invalid(err)
			}
			if ref.ID == "" {
				return notLinkID(ref)
			}
			return runLink(cmd.Context(), p, d, ref)
		},
	}
}

// notLinkID refuses a link where an id is wanted. The link may carry a
// secret, so only its label is repeated.
func notLinkID(ref share.Ref) *problem {
	return misuse("link takes a share id, not a link").remedy("try ", bright("purlview link "+ref.String()))
}

func runLink(ctx context.Context, p *output.Printer, d Deps, ref share.Ref) error {
	cred, err := loadCredential(d)
	if err != nil {
		return err
	}
	shares, err := accountShares(ctx, d, cred)
	if err != nil {
		var pr *problem
		if errors.As(err, &pr) {
			return pr
		}
		// Purlview is unreachable; this device still knows its own shares.
		if shares = localShares(ctx, d); len(shares) == 0 {
			return unreachable(err)
		}
	}
	for _, sh := range shares {
		if sh.ID == ref.ID && sh.URL != "" {
			p.Link(sh.URL)
			return nil
		}
	}
	if err != nil {
		return unreachable(err)
	}
	return noShare(ref, nil)
}
