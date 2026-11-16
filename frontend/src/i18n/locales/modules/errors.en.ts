// errors (en-US) — must mirror modules/errors.ts key-for-key.
export const errors = {
  UNAUTHENTICATED: 'Not signed in or the session has expired — please sign in again',
  INVALID_CREDENTIALS: 'Incorrect username or password',
  USER_DISABLED: 'This account is disabled. Contact your administrator.',
  FORBIDDEN: 'You do not have permission to do that',
  NOT_FOUND: 'Not found or already deleted',
  PROJECT_NOT_FOUND: 'Project not found or already deleted',
  TOKEN_NOT_FOUND: 'Token not found or already deleted',
  PATH_NOT_FOUND: 'Project local path is missing — fix it in project settings or re-import the repository',
  BAD_REQUEST: 'Malformed request',
  MISSING_FIELDS: 'Missing required fields',
  INVALID: 'Invalid parameter',
  INVALID_REQUEST: 'Invalid request parameter',
  INVALID_STATUS: 'Invalid status',
  INVALID_LOCALE: 'Unsupported language',
  NO_SESSION: 'No session yet — start the stage first',
  INTERNAL: 'Internal server error',
  INTERNAL_ERROR: 'Internal server error',
  DB_ERROR: 'Database error',
  UNKNOWN: 'Unknown error',
  WIKI_ONLY: 'A knowledge-base requirement can only be scheduled for knowledge-doc generation',
  NOT_WIKI: 'Knowledge-doc generation can only be scheduled for knowledge-base requirements',
  WIKI_NOT_DEVELOPABLE: 'A knowledge-base requirement cannot enter the development stage',
  // Recovery paths for a Review report's structured result
  // (handler/review_reparse.go, handler/review_ai_parse.go).
  CONTRACT_UNAVAILABLE: 'The report has neither a JSON contract block nor parsable severity sections',
  FINDINGS_UNAVAILABLE: 'The findings section of the report is malformed',
  REVIEW_NOT_FINISHED: 'This Review has not produced its final report yet',
  NOT_AVAILABLE: 'This feature is not enabled in the current environment',
  AI_PARSE_IN_PROGRESS: 'This Review is already being AI-parsed — please wait for it to finish',
  // Manual-edit protection for the project-level CLAUDE.md: a destructive
  // replace (regenerate / import from disk) needs a confirmation while the
  // document carries hand-edited content.
  MANUAL_EDIT_PROTECTED: 'This CLAUDE.md contains hand-edited content — regenerating or importing would overwrite it. Confirm and retry (the old version is kept in the history and can be restored)',
  REVISION_NOT_FOUND: 'That version does not exist (or was pruned)',
  NETWORK: 'Network error, please try again later',
} as const;

export default errors;
