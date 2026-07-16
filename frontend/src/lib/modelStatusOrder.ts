export function mergeFilteredModelOrder(
  fullOrder: readonly string[],
  filteredOrder: readonly string[],
  activeId: string,
  overId: string,
  persistedOrder: readonly string[] = [],
): string[] {
  const oldIndex = filteredOrder.indexOf(activeId)
  const newIndex = filteredOrder.indexOf(overId)
  const reorderedSubset = [...filteredOrder]
  if (oldIndex !== -1 && newIndex !== -1 && oldIndex !== newIndex) {
    const [movedItem] = reorderedSubset.splice(oldIndex, 1)
    reorderedSubset.splice(newIndex, 0, movedItem)
  }

  const reorderedNames = new Set(reorderedSubset)
  let reorderedIndex = 0
  const mergedOrder = fullOrder.map(modelName => (
    reorderedNames.has(modelName)
      ? reorderedSubset[reorderedIndex++]
      : modelName
  ))

  const knownNames = new Set(mergedOrder)
  persistedOrder.forEach(modelName => {
    if (!knownNames.has(modelName)) {
      knownNames.add(modelName)
      mergedOrder.push(modelName)
    }
  })
  return mergedOrder
}
