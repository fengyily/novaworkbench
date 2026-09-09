import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
// i18n must initialize before App mounts so the first render is already in
// the user's language (side-effect import — see i18n/index.ts).
import './i18n'
import './index.css'
import App from './App.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
