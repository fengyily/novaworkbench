// claudeDoc — the project-level CLAUDE.md panel on the project detail page
// (English mirror of modules/claudeDoc.ts — the two must stay key-for-key).
export const claudeDoc = {
  title: 'Project CLAUDE.md',
  loading: 'Loading…',

  // Empty state
  emptyTitle: 'No CLAUDE.md has been generated for this project yet.',
  emptyHint: 'Once generated, it is attached automatically to requirement analysis, design, coding and sub-tasks, so the model stops re-discovering the project layout every time.',

  // Header badges
  sourceLabel: 'Source',
  sourceGenerated: 'AI generated',
  sourceManual: 'Hand-edited',
  sourceImported: 'Imported from disk',
  lastGenerated: 'Last generated: {{time}}',
  neverGenerated: 'Never generated',
  sizeLabel: '{{n}} bytes',

  // Disk hint — the on-disk CLAUDE.md and the managed copy are two documents,
  // and the user can only see the managed one from this panel.
  diskExists: 'A CLAUDE.md already exists in the repository directory and the Claude CLI loads it automatically; the version on this panel wins.',
  diskDiffers: 'The CLAUDE.md on disk no longer matches the managed copy{{at}}; the managed copy is still what gets injected — import the disk version to update it.',
  diskModifiedAt: ' (disk version: {{time}})',
  diskMissing: 'There is no CLAUDE.md in the repository directory; the managed copy is what gets injected.',
  // The injection channel — how the managed document reaches the model.
  // "native" = the Claude CLI's own project-memory channel; "prompt" = the
  // document is spliced into the user prompt.
  injectionChannelNative: 'Native memory',
  injectionChannelPrompt: 'Prompt fallback',
  injectionChannelLabel: 'Injection channel',
  // In native mode the repository's own CLAUDE.md is kept OUT of the
  // context, so an edit there has no effect until it is imported.
  singleSourceHint: 'Only the copy on this panel is loaded into the session; the CLAUDE.md in the repository directory does not take part — import it to apply your changes there.',
  importFromDisk: 'Import from disk',

  // Actions
  generate: '🚀 Generate CLAUDE.md',
  regenerate: 'Regenerate',
  edit: 'Edit',
  save: 'Save',
  saving: 'Saving…',
  cancel: 'Cancel',
  viewSource: 'View source',
  viewRendered: 'Rendered view',
  remove: 'Delete',
  removeConfirm: 'Delete the project-level CLAUDE.md? Requirement analysis, design and coding will no longer carry the project conventions automatically.',
  savedOk: 'Saved ✓',

  // Generation
  focusLabel: 'Generation focus (optional)',
  focusPlaceholder: 'e.g. focus on the backend layering and the database migrations',
  generating: 'Generating…',
  progressTitle: 'Generation progress',
  clearLog: 'Clear',
  generateFailed: 'Generation failed',

  // Editing
  editHint: 'A hand edit does not change the "last generated" time.',

  // Manual-edit protection — a destructive replace (regenerate / import
  // from disk) has to be confirmed first.
  manualEdited: 'Hand-edited',
  overwriteManualConfirm: 'This CLAUDE.md contains hand-edited content and continuing will overwrite it (the old version is kept in the history and can be restored). Continue?',

  // Version history
  historyShow: 'Version history ({{n}})',
  historyHide: 'Hide history',
  historyEmpty: 'No versions yet. Every regeneration / import stores the version it replaced here.',
  historyRestore: 'Restore this version',
  restoreConfirm: 'Restore this version? The current version is stored in the history first, so you can restore it back.',
  restoredOk: 'Version restored ✓',
  reasonRegenerate: 'Before regeneration',
  reasonImport: 'Before disk import',
  reasonRestore: 'Before restore',
};
