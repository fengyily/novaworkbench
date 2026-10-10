// errors (en-US) — must mirror modules/errors.ts key-for-key.
export const errors = {
  UNAUTHENTICATED: 'Not signed in or the session has expired — please sign in again',
  INVALID_CREDENTIALS: 'Incorrect username or password',
  USER_DISABLED: 'This account is disabled. Contact your administrator.',
  FORBIDDEN: 'You do not have permission to do that',
  NOT_FOUND: 'Not found or already deleted',
  PROJECT_NOT_FOUND: 'Project not found or already deleted',
  TOKEN_NOT_FOUND: 'Token not found or already deleted',
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
  NETWORK: 'Network error, please try again later',
} as const;

export default errors;
