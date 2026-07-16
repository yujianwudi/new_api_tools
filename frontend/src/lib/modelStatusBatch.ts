export function normalizeModelStatusMaxBatch(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isInteger(value) && value > 0
    ? value
    : fallback
}

export function chunkModelNames(modelNames: readonly string[], maxBatch: number): string[][] {
  if (!Number.isInteger(maxBatch) || maxBatch < 1) {
    throw new Error('Model status max batch must be a positive integer')
  }
  const chunks: string[][] = []
  for (let offset = 0; offset < modelNames.length; offset += maxBatch) {
    chunks.push(modelNames.slice(offset, offset + maxBatch))
  }
  return chunks
}
