//go:build devbuild

package main

import (
	"fmt"

	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/fakedrive"
)

type seeded struct{ folders, files int }

// seed fills the drive with a synthetic tree:
//
//	/Projects/Example Tower/{Drawings,Models,Photos}  a few small files each
//	/Projects/Sample Park/Reports
//	/Large folder/                                    bigFolder small files
//	/Large file/Generated 5 GiB.bin                   generated on read
//	/Managed outputs/                                 writes refused: managed (423)
//	/Issued for construction/                         sealed: writes refused (403)
func seed(fd *fakedrive.Server, bigFolder int, virtualSize int64) seeded {
	var n seeded
	folder := func(parent, name string) string { n.folders++; return fd.AddFolder(parent, name) }
	file := func(parent, name, text string) { n.files++; fd.AddFile(parent, name, []byte(text)) }
	root := fd.RootID()

	projects := folder(root, "Projects")
	tower := folder(projects, "Example Tower")
	for _, sub := range []string{"Drawings", "Models", "Photos"} {
		id := folder(tower, sub)
		for i := 1; i <= 3; i++ {
			file(id, fmt.Sprintf("%s %02d.txt", sub, i), fmt.Sprintf("Synthetic %s file %d for testing.\n", sub, i))
		}
	}
	park := folder(projects, "Sample Park")
	reports := folder(park, "Reports")
	file(reports, "Weekly report.txt", "Synthetic weekly report.\n")
	file(root, "Read me.txt", "This drive is a fake for testing the desktop client. Everything here is synthetic.\n")

	large := folder(root, "Large folder")
	for i := 1; i <= bigFolder; i++ {
		file(large, fmt.Sprintf("item-%05d.txt", i), fmt.Sprintf("item %d\n", i))
	}
	if virtualSize > 0 {
		big := folder(root, "Large file")
		n.files++
		fd.AddVirtualFile(big, fmt.Sprintf("Generated %s.bin", sizeName(virtualSize)), virtualSize)
	}

	managed := folder(root, "Managed outputs")
	file(managed, "Generated summary.txt", "Kept up to date by an example integration.\n")
	fd.RefuseWrites(managed, driveapi.CodeManaged)

	sealed := folder(root, "Issued for construction")
	file(sealed, "Issued drawing 01.txt", "Sealed: this folder takes no changes.\n")
	fd.RefuseWrites(sealed, driveapi.CodeSealed)
	return n
}

func sizeName(b int64) string {
	if b >= 1<<30 && b%(1<<30) == 0 {
		return fmt.Sprintf("%d GiB", b>>30)
	}
	if b >= 1<<20 {
		return fmt.Sprintf("%d MiB", b>>20)
	}
	return fmt.Sprintf("%d bytes", b)
}
