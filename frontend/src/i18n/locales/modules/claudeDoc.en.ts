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

  // Disk hint
  diskExists: 'A CLAUDE.md already exists in the repository directory and the Claude CLI loads it automatically; the version on this panel wins.',
  diskMissing: 'There is no CLAUDE.md in the repository directory.',
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
};
