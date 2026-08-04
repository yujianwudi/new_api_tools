import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

function TestSurface() {
  return <button type="button">测试环境可用</button>
}

describe('frontend test harness', () => {
  it('renders React in jsdom with jest-dom matchers', () => {
    render(<TestSurface />)
    expect(screen.getByRole('button', { name: '测试环境可用' })).toBeVisible()
  })
})
