import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App.tsx'
import './index.css'
import { AuthProvider } from './contexts/AuthContext'
import { ToastProvider } from './components'
import { initializeStorage } from './services/indexedDB'

// Legacy localStorage secrets are removed synchronously before
// initializeStorage reaches its first await. IndexedDB migration then runs in
// parallel so a blocked or unavailable optional store cannot blank the app.
void initializeStorage().catch((error) => {
  console.error('Browser storage initialization failed:', error)
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <AuthProvider>
      <ToastProvider>
        <App />
      </ToastProvider>
    </AuthProvider>
  </React.StrictMode>,
)
