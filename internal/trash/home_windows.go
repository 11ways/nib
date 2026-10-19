package trash

// homeTrash is the trash in the freedesktop layout under nib's data folder, which trash list and trash restore serve.
func homeTrash() trashDir { return freedesktop(Home(), "") }

// topTrash is never taken: a file on another volume is copied into the home trash.
func topTrash(string, bool) (trashDir, bool) { return trashDir{}, false }
