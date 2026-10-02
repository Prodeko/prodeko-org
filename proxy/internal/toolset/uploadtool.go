package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/upload"
)

// The tool side of the image upload. The bytes never come through here: this
// mints the token, names the link, and receives the saved file into the
// change through SaveImage. See the upload package for why.

// uploadResourceURI is the widget's address under the MCP Apps extension. A
// host that speaks the extension renders it in the chat; every other client
// gets the same document as a plain page at the upload URL.
const uploadResourceURI = "ui://prodeko-editor/upload"

// imagesRoot is where uploads land in the change, inside the fence's
// writable set.
const imagesRoot = "site/assets/images/"

// uploadToolMeta marks begin_image_upload as carrying a widget.
var uploadToolMeta = json.RawMessage(`{"ui":{"resourceUri":"` + uploadResourceURI + `"}}`)

// UIResources is what the toolset asks the transport to serve: the upload
// widget, with a content security policy that allows connecting back to this
// server and nothing else.
func UIResources(publicURL string) []mcpserver.Resource {
	meta, _ := json.Marshal(map[string]any{
		"ui": map[string]any{
			"csp": map[string]any{"connectDomains": []string{publicURL}},
		},
	})
	return []mcpserver.Resource{{
		URI:         uploadResourceURI,
		Name:        "Image upload",
		Description: "Drop a JPEG or PNG into the current change.",
		MimeType:    "text/html",
		Text:        upload.PageHTML,
		Meta:        meta,
	}}
}

func (t *Toolset) beginImageUpload(ctx context.Context, id mcpserver.Identity, args json.RawMessage) (string, error) {
	a, err := decode[beginImageUploadArgs](args)
	if err != nil {
		return "", fmt.Errorf("%s: %w", ToolBeginImageUpload, err)
	}
	if t.uploads == nil || t.publicURL == "" {
		return "", fmt.Errorf("%s: this server has no upload endpoint configured", ToolBeginImageUpload)
	}
	user, err := userOf(id)
	if err != nil {
		return "", err
	}
	// The change is opened now, named after the purpose, so the upload has
	// somewhere to land even when this is the first thing the person asked.
	if _, err := t.writingUser(user, strings.TrimSpace(a.Purpose)); err != nil {
		return "", err
	}
	token, err := t.uploads.Mint(user)
	if err != nil {
		return "", err
	}
	link := t.publicURL + upload.Path + "?token=" + token
	return fmt.Sprintf("Ask the person to open this link and drop the image in:\n\n%s\n\n"+
		"It works once and expires in 15 minutes; JPEG or PNG, at most 5 MB. The file lands in this change "+
		"under %s and the upload page tells them the exact path — ask them to say when it is done, then use "+
		"that path in the content.", link, imagesRoot), nil
}

// SaveImage is the upload handler's way into the change. The name is already
// clean (the handler made it) but the fence is still asked, because every
// write goes through it or it is not a fence.
func (t *Toolset) SaveImage(user, name string, data []byte) (string, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "..") {
		return "", fmt.Errorf("toolset: %q is not a bare file name", name)
	}
	c, err := t.writingUser(user, "kuva")
	if err != nil {
		return "", err
	}
	rel := imagesRoot + name
	if err := c.Fence().CheckWrite(rel, int64(len(data))); err != nil {
		return "", err
	}
	if err := c.WriteFile(rel, data); err != nil {
		return "", err
	}
	return rel, nil
}

// Prompts is the pre-written entry points the transport lists. One for now:
// the most frequent media-team task, spelled out so the model starts on the
// right file instead of hunting for it.
func Prompts() []mcpserver.Prompt {
	return []mcpserver.Prompt{{
		Name:        "post-announcement",
		Description: "Päivitä etusivun Ajankohtaista-nosto (fi ja en).",
		Text: "Update the front page announcement on prodeko.org.\n\n" +
			"The announcement is the `announcement:` block in the front matter of site/content/fi/_index.md " +
			"(kicker, title, body, linkText, and a link). The English side is the same block in " +
			"site/content/en/_index.md; the pages pair by translationKey. Read both, make the change in " +
			"Finnish first, keep the English side in step or say plainly that you left it alone and why. " +
			"Show invented copy to the person before submitting. Then build, and submit with a Finnish title.",
	}}
}
