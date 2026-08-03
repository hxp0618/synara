/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_ENABLE_DEV_LOGIN?: string;
  readonly VITE_TENANT_APP_URL?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
