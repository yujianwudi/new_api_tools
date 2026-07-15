import tseslintPlugin from '@typescript-eslint/eslint-plugin'
import tseslintParser from '@typescript-eslint/parser'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'

export default [
  {
    ignores: ['dist/**', 'node_modules/**'],
    linterOptions: {
      reportUnusedDisableDirectives: false,
    },
  },
  {
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 'latest',
      parser: tseslintParser,
      parserOptions: {
        ecmaFeatures: { jsx: true },
        sourceType: 'module',
      },
      sourceType: 'module',
    },
    plugins: {
      '@typescript-eslint': tseslintPlugin,
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      'no-debugger': 'error',
      'no-duplicate-imports': 'error',
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
      'react-refresh/only-export-components': ['error', { allowConstantExport: true }],
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          caughtErrors: 'none',
          varsIgnorePattern: '^_',
        },
      ],
    },
  },
  {
    // These legacy modules intentionally co-locate components with helpers/hooks.
    files: [
      'src/components/History.tsx',
      'src/components/ModelStatusEmbed.tsx',
      'src/components/Toast.tsx',
      'src/components/UserAnalysisDialog.tsx',
      'src/components/ui/badge.tsx',
      'src/components/ui/button.tsx',
      'src/contexts/AuthContext.tsx',
    ],
    rules: {
      'react-refresh/only-export-components': 'off',
    },
  },
  {
    // Existing imports are split between type/value declarations in these modules.
    files: ['src/components/Toast.tsx', 'src/components/TopUpAudit.tsx'],
    rules: {
      'no-duplicate-imports': 'off',
    },
  },
  {
    // Dependency cleanup for these long-lived effects is tracked as component work.
    files: ['src/components/RealtimeRanking.tsx'],
    rules: {
      'react-hooks/exhaustive-deps': 'off',
    },
  },
]
