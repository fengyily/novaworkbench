// login (en-US) — must mirror modules/login.ts key-for-key.
export const login = {
  title: 'Sign in',
  subtitle: 'Role-based access control is enabled. Sign in with your account.',
  username: 'Username',
  password: 'Password',
  submit: 'Sign in',
  submitting: 'Signing in…',
  failed: 'Sign-in failed',
  hintPrefix:
    'The admin account is created automatically on first start; its password is printed in the backend start log (',
  hintSuffix: ').',
} as const;

export default login;
