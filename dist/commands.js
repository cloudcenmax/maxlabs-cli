                                                           

export const thinkingLevels                              = ["minimal", "low", "medium", "high", "default"];

export function resolveThinkingLevel(values          )                                        {
  return [...values].reverse().find((value)                                     =>
    thinkingLevels.includes(value                             ));
}

export function defaultModel(requested        , models             )         {
  if (requested.trim()) return requested.trim();
  return models.find((model) => model.id === "worker")?.id || models[0]?.id || "auto";
}


//# sourceURL=../src/commands.ts