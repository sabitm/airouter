package claudecode

// Tool renaming and decoy injection were removed. Claude Code 2.1.303 does not
// append _ide to caller tools and does not inject unavailable native tools.
// A caller tool whose name already ends in _ide is preserved.
