package web

import (
	"net/http"
	"strconv"
	"strings"
)

// The artifact pages answer two questions the transcript cannot: what did this
// session do to this file, and what did the file look like at each step.
//
// The content is served from the stored event body rather than fetched from the
// machine that produced it. That is the only honest option — the file on disk
// today is whatever happened since, and rendering it here would quietly present
// the present as the past.

type artifactView struct {
	Page     Page
	Artifact Artifact
	Versions []ArtifactVersion
	// Selected is the version being shown, and Content its text. When no version
	// is requested the newest is selected, because the question people arrive
	// with is almost always "what does it look like now".
	Selected ArtifactVersion
	Content  string
	// Truncated reports that Content is not the whole file. A viewer that
	// silently shows the first part of a file is one that produces confident
	// wrong conclusions about what is in the rest.
	Truncated bool
	// SessionURL goes back to the transcript this came from.
	SessionURL string
}

// maxRendered bounds what is put in one HTML page.
//
// Generated files run to megabytes, and a page that embeds one is a tab that
// stops responding on the machine of whoever clicked it. The bound is stated in
// the page rather than applied silently.
const maxRendered = 512 << 10

// VersionURL is the link to one version of this artifact.
func (v artifactView) VersionURL(av ArtifactVersion) string {
	return "/artifacts/" + strconv.FormatInt(v.Artifact.ID, 10) + "?v=" + av.EventID
}

// IsSelected reports whether a version is the one being shown, so the list can
// mark it rather than leaving the reader to work out which pane belongs to which
// row.
func (v artifactView) IsSelected(av ArtifactVersion) bool {
	return av.EventID == v.Selected.EventID
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	v, ok := s.require(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, v)
		return
	}

	art, versions, err := s.data.Artifact(r.Context(), v, id)
	if err != nil {
		s.readError(w, r, v, err)
		return
	}

	view := artifactView{
		Page:       s.page(v, art.Name(), "sessions"),
		Artifact:   art,
		Versions:   versions,
		SessionURL: "/sessions/" + art.SessionID,
	}

	// Pick the version to show: the one asked for, else the newest. An unknown
	// id falls back to the newest rather than 404ing, because the id in the
	// query is a detail of this page and not something a reader typed.
	if len(versions) > 0 {
		view.Selected = versions[0]
		if want := strings.TrimSpace(r.URL.Query().Get("v")); want != "" {
			for _, av := range versions {
				if av.EventID == want {
					view.Selected = av
					break
				}
			}
		}
		body, err := s.data.ArtifactContent(r.Context(), v, id, view.Selected.EventID)
		if err != nil {
			// The history is still worth showing without the body: it says when
			// the file changed and how big it was each time.
			s.log.Error("read artifact content", "artifact", id, "event", view.Selected.EventID, "err", err)
		} else if len(body) > maxRendered {
			view.Content, view.Truncated = body[:maxRendered], true
		} else {
			view.Content = body
		}
	}

	s.rnd.render(w, http.StatusOK, "artifact.html", view)
}
