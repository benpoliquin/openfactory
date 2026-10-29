package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

// Local inspection never grants model disclosure, execution, or a browser
// identity. Canonical export returns the exact immutable reviewed source bytes.
func runIntake(ctx context.Context, c model.Config, db *store.Store, args []string, out io.Writer) error {
	emit := func(v any) error { enc := json.NewEncoder(out); enc.SetIndent("", "  "); return enc.Encode(v) }
	if len(args) == 0 {
		return errors.New("usage: intake status [REQUEST_ID | --after ID --limit NUMBER] | export REQUEST_ID --revision NUMBER [--output FILE]")
	}
	command, args := args[0], args[1:]
	switch command {
	case "status":
		if len(args) == 0 || strings.HasPrefix(args[0], "--") {
			fs := flag.NewFlagSet("intake status", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			after := fs.String("after", "", "exclusive request ID cursor")
			limit := fs.Int("limit", c.Intake.HistoryDefault, "maximum request count")
			if err := fs.Parse(args); err != nil {
				return err
			}
			if fs.NArg() != 0 || *limit < 1 || *limit > c.Intake.HistoryMax {
				return errors.New("intake status requires --limit between 1 and intake.history_max and no request ID with list options")
			}
			items, err := db.ListIntakeRequests(ctx, "", "", *after, *limit)
			if err != nil {
				return err
			}
			next := ""
			if len(items) == *limit {
				last := items[len(items)-1].ID
				remainder, err := db.ListIntakeRequests(ctx, "", "", last, 1)
				if err != nil {
					return err
				}
				if len(remainder) > 0 {
					next = last
				}
			}
			if items == nil {
				items = []model.IntakeRequest{}
			}
			return emit(struct {
				Items      []model.IntakeRequest `json:"items"`
				NextCursor string                `json:"next_cursor,omitempty"`
				HasMore    bool                  `json:"has_more"`
				Limit      int                   `json:"limit"`
			}{items, next, next != "", *limit})
		}
		if len(args) != 1 {
			return errors.New("usage: intake status REQUEST_ID (list options cannot be combined with an ID)")
		}
		request, err := db.IntakeRequest(ctx, args[0])
		if err != nil {
			return err
		}
		drafts, err := db.IntakeDrafts(ctx, request.ID)
		if err != nil {
			return err
		}
		questions, err := db.IntakeQuestions(ctx, request.ID)
		if err != nil {
			return err
		}
		links, err := db.IntakeLinks(ctx, request.ID)
		if err != nil {
			return err
		}
		return emit(map[string]any{"request": request, "drafts": drafts, "questions": questions, "links": links})
	case "export":
		if len(args) != 3 && len(args) != 5 || len(args) < 3 || args[1] != "--revision" || len(args) == 5 && (args[3] != "--output" || args[4] == "") {
			return errors.New("usage: intake export REQUEST_ID --revision NUMBER [--output FILE]")
		}
		revision, err := strconv.Atoi(args[2])
		if err != nil || revision < 1 {
			return errors.New("draft revision must be a positive integer")
		}
		draft, err := db.IntakeDraft(ctx, args[0], revision)
		if err != nil {
			return err
		}
		if len(args) == 3 {
			_, err = io.WriteString(out, draft.CanonicalSource)
			return err
		}
		file, err := os.OpenFile(args[4], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if _, err = io.WriteString(file, draft.CanonicalSource); err != nil {
			return err
		}
		if err = file.Sync(); err != nil {
			return err
		}
		return emit(map[string]any{"request_id": args[0], "revision": revision, "output": args[4], "source_sha": draft.SourceSHA})
	default:
		return errors.New("usage: intake status [REQUEST_ID | --after ID --limit NUMBER] | export REQUEST_ID --revision NUMBER [--output FILE]")
	}
}
