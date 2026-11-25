package gtfs

import (
	"io"

	"github.com/MKuranowski/WarsawGTFS/realtime/statickey"
)

// StaticKey returns the key identifying this exact static dataset.
//
// The key is the feed_info.txt feed_version when the feed declares one.
// Otherwise it falls back to "sha256:<digest>" of the underlying ZIP bytes.
// File-backed and URL/reader-backed GTFS objects are supported; in the latter
// case the temporary file holding the downloaded bytes is hashed.
func (g *Gtfs) StaticKey() (string, error) {
	if f := g.GetZipFileByName(statickey.FeedInfoName); f != nil {
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		feedVersion, err := statickey.ReadFeedVersion(rc)
		rc.Close()
		if err == nil && feedVersion != "" {
			return feedVersion, nil
		}
		// Unreadable/missing feed_version - fall through to the hash.
	}

	hash, err := g.contentHash()
	if err != nil {
		return "", err
	}
	return statickey.HashPrefix + hash, nil
}

// contentHash streams the underlying ZIP file through SHA-256.
func (g *Gtfs) contentHash() (string, error) {
	// Seek back to the beginning first: the file may have been read already.
	if seeker, ok := g.fileObj.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	hash, err := statickey.HashReader(g.fileObj.(io.Reader))
	if err != nil {
		return "", err
	}
	// Leave the reader at the start again for any subsequent access.
	if seeker, ok := g.fileObj.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
	}
	return hash, nil
}
