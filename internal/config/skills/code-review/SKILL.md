---
name: code-review
description: Guidelines for reviewing code in pull requests
---

## Review checklist
1. Correctness: Does the code do what it intends? Are edge cases handled?
2. Security: Are there injection vulnerabilities, exposed secrets, or unsafe operations?
3. Performance: Are there obvious inefficiencies or unnecessary allocations?
4. Error handling: Are errors checked and handled appropriately?
5. Testing: Are there tests for the new functionality?

## Output format
For each issue found, include:
- File path and line number
- Severity (blocker, major, minor)
- Description of the issue
- Suggested fix
