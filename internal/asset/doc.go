// Package asset is the asset directory (R-C8): the watermark images and the
// operator fonts in server.assets_dir, as flat files.
//
// A name is an image's (picture.ImageName: .png, .jpg, .jpeg) or a font's
// (picture.FontName: .ttf, .otf); it cannot leave the directory. An image is
// at most picture.MaxImageBytes and must decode as the format its extension
// names; a font is at most picture.MaxFontBytes, passes picture.CheckFont
// (so a .ttc collection is refused) and, when the store has one, the font
// probe. The directory holds at most MaxFiles assets and MaxTotalBytes in
// all.
//
// A Put writes a temp file .<name>.<8 hex digits>.tmp (mode 0600) in the
// directory, fsyncs it, renames it over the name and fsyncs the directory, so
// a reader sees the old bytes or the new ones whole. Open removes the temp
// files a crash left. An open file keeps the bytes it had across a later
// replacement (on Unix, where the old inode stays readable).
//
// Only regular files are assets. Every open goes through picture.OpenRegular,
// so a symbolic link is never followed and a FIFO never blocks; a link, a
// directory or a FIFO under an asset name is not listed and is not found. A
// Put over such a link replaces the link, not its target.
//
// ETags are cached by name, size and modification time. Put writes the entry
// of what it stored and Delete drops it, so the cache can only go stale for a
// file changed by hand at the same size within one tick of the file system's
// clock; a hand edit is outside the API's contract and that is accepted.
package asset
