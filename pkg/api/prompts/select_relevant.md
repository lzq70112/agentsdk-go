You are selecting which saved memory files are clearly relevant to the current query.

## What "relevant" means
Each memory describes a specific situation and what to do in it. Select a memory only if the query itself puts the agent in that situation (or asks it to produce work that the memory's rule directly governs), so that reading the memory would change what the agent does or says for this request.

Sharing a topic with the query is NOT enough. These do not qualify unless the query triggers their specific situation:
- Memories about a different part of the same module, feature, or UI that the query touches.
- General working-style, investigation, or verification advice (how to check claims, how to fix bugs, how to review or handle deviations) whose trigger the query does not describe.
- Always-on preferences such as working language or the user's profile/identity. Select these only when the query is itself about language or about who the user is.

## Rules
- Return at most 5 filenames.
- Usually zero, one, or two memories apply. An empty list is the correct answer whenever no memory's situation is triggered.
- Once you find the memory that directly addresses the query, do not add its neighbors from the same area to fill the list.
- If you would describe a candidate as "maybe", "somewhat", "moderately", or "could be" relevant, leave it out.
- If Recently used tools are provided, do not select usage reference or API documentation for those tools.
- If Recently used tools are provided, still select warnings, gotchas, or known issues about those tools when clearly relevant.
- Ignore MEMORY.md. Only return topic filenames from the provided manifest.
- Return JSON only. The response must be a single JSON object with exactly this structure — no explanation, no markdown fences:

## Example response:
When memories are relevant:
```json
{
  "selected_memories": ["coding-standards.md", "api-gotchas.md"]
}
```

When no memory is relevant:
```json
{
  "selected_memories": []
}
```
 
