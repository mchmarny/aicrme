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
const recipe = {
  name: 'h100-eks-ubuntu-training',
  version: '0.19.0',
  componentCount: 16,
  components: [
    { name: 'gpu-operator', kind: 'Helm', version: 'v26.3.3', namespace: 'gpu-operator' },
    { name: 'kai-scheduler', kind: 'Helm', version: 'v0.14.1', namespace: 'kai-scheduler' },
  ],
}

describe('Recommend', () => {
  it('asks exactly two questions', () => {
    render(<Recommend options={options} recipe={null} onDecide={vi.fn()} />)
    expect(screen.getAllByRole('radiogroup')).toHaveLength(2)
  })

  it('submits both decisions together', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} recipe={null} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByLabelText('kubeflow'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).toHaveBeenCalledWith({ intent: 'training', platform: 'kubeflow' })
  })

  it('does not submit until both are chosen', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} recipe={null} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).not.toHaveBeenCalled()
  })

  it('folds the resolved component list behind a summary', () => {
    render(<Recommend options={options} recipe={recipe} onDecide={vi.fn()} />)
    expect(screen.getByText(/16 components/)).toBeDefined()
    expect(screen.getByText(/gpu-operator v26.3.3/)).toBeDefined()
  })

  it('shows the union of every intent\'s platforms before an intent is chosen', () => {
    render(<Recommend options={options} recipe={null} onDecide={vi.fn()} />)
    for (const p of options.platforms) {
      expect(screen.getByLabelText(p)).toBeDefined()
    }
  })

  it('narrows the platform choices to the ones proven for the selected intent', () => {
    render(<Recommend options={options} recipe={null} onDecide={vi.fn()} />)
    fireEvent.click(screen.getByLabelText('training'))
    expect(screen.getByLabelText('kubeflow')).toBeDefined()
    expect(screen.getByLabelText('slurm')).toBeDefined()
    expect(screen.queryByLabelText('runai')).toBeNull()
    expect(screen.queryByLabelText('dynamo')).toBeNull()
  })

  it('clears a platform choice that the newly selected intent narrows out', () => {
    const onDecide = vi.fn()
    render(<Recommend options={options} recipe={null} onDecide={onDecide} />)
    fireEvent.click(screen.getByLabelText('inference'))
    fireEvent.click(screen.getByLabelText('runai'))
    fireEvent.click(screen.getByLabelText('training'))
    fireEvent.click(screen.getByRole('button', { name: /continue/i }))
    expect(onDecide).not.toHaveBeenCalled()
  })
})
