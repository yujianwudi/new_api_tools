export function normalizeModelStatusMaxBatch(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isInteger(value) && value > 0
    ? value
    : fallback
}

export const MODEL_STATUS_BATCH_MAX_CONCURRENCY = 3

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

export async function mapWithConcurrency<Item, Result>(
  items: readonly Item[],
  maxConcurrency: number,
  worker: (item: Item, index: number) => Promise<Result>,
): Promise<Result[]> {
  if (!Number.isInteger(maxConcurrency) || maxConcurrency < 1) {
    throw new Error('Concurrency limit must be a positive integer')
  }
  if (items.length === 0) return []

  const results = new Array<Result>(items.length)
  let nextIndex = 0
  let failed = false

  const runWorker = async () => {
    while (!failed) {
      const index = nextIndex
      if (index >= items.length) return
      nextIndex += 1
      try {
        results[index] = await worker(items[index], index)
      } catch (error) {
        failed = true
        throw error
      }
    }
  }

  const workerCount = Math.min(maxConcurrency, items.length)
  await Promise.all(Array.from({ length: workerCount }, runWorker))
  return results
}
