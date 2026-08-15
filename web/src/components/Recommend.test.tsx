import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Recommend } from './Recommend'

// platformsByIntent, not the flat platforms list, is what Recommend must
// render from -- see the doc comment on internal/api/options.go's
// handleOptions. The flat list is kept here only for shape-fidelity with the
// real /api/options response; a test that fed Recommend a platform absent
// from platformsByIntent would not catch a regression back to driving the
// UI off the union.
const options = {
  intents: ['training', 'inference'],
  platforms: ['kubeflow', 'slurm', 'runai', 'dynamo'],
  platformsByIntent: { training: ['kubeflow', 'slurm'], inference: ['runai', 'dynamo'] },
  provisional: false,
}

describe('Recommend', () => {
  it('asks exactly two questions', () => {
    render(<Recommend options={options} onDecide={vi.fn()} />)
    expect(screen.getAllByRole('radiogroup')).toHaveLength(2)
  })

  it('submits both decisions together', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByLabelText('kubeflow'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).toHaveBeenCalledWith({ intent: 'training', platform: 'kubeflow' })
  })

  it('does not submit until both are chosen', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).not.toHaveBeenCalled()
  })

  it('shows the union of every intent\'s platforms before an intent is chosen', () => {
    render(<Recommend options={options} onDecide={vi.fn()} />)
    for (const p of options.platforms) {
      expect(screen.getByLabelText(p)).toBeDefined()
    }
  })

  it('narrows the platform choices to the ones proven for the selected intent', () => {
    render(<Recommend options={options} onDecide={vi.fn()} />)
    fireEvent.click(screen.getByLabelText('training'))
    expect(screen.getByLabelText('kubeflow')).toBeDefined()
    expect(screen.getByLabelText('slurm')).toBeDefined()
    expect(screen.queryByLabelText('runai')).toBeNull()
    expect(screen.queryByLabelText('dynamo')).toBeNull()
  })

  it('clears a platform choice that the newly selected intent narrows out', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('inference'))
    fireEvent.click(screen.getByLabelText('runai'))
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).not.toHaveBeenCalled()
  })
})
