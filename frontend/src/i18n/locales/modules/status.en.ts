// status (en-US) — must mirror modules/status.ts key-for-key.
export const status = {
  req: {
    draft: '📝 Draft',
    analyzing: '🔍 Analyzing',
    designing: '📐 Designing',
    designed: '📐 Designed',
    developing: '🚀 Developing',
    done: '✅ Done',
    archived: '📦 Archived',
  },
  kind: {
    issue: '🐛 Issue',
    requirement: '📋 Requirement',
    idea: '💡 Idea',
  },
  kindFilter: {
    issue: 'Issue',
    requirement: 'Requirement',
    idea: 'Idea',
  },
  kindShort: {
    issue: 'Issue',
    requirement: 'Requirement',
    idea: 'Idea',
  },
  kindHint: {
    issue: 'Needs: repro steps / error output / expected behaviour',
    requirement: 'Needs: background / goal / feature outline / acceptance criteria',
    idea: 'One sentence is enough — AI helps you assess feasibility',
  },
  kindPlaceholder: {
    issue: 'Describe the symptom / repro steps / error message…',
    requirement: 'Describe the feature you want in plain language…',
    idea: 'Write down your idea or inspiration; AI will assess feasibility…',
  },
  kindChatPlaceholder: {
    issue: 'Paste URLs, page elements, error screenshots, or extra repro steps... type @ to reference a Skill',
    requirement: 'Paste URLs, describe page elements, or reply to AI questions... type @ to reference a Skill',
    idea: 'Share your doubts, concerns or alternatives... type @ to reference a Skill',
  },
  stage: {
    analyst: 'Analysis',
    architect: 'Design',
    developer: 'Development',
  },
  step: {
    requirement_create: 'Organize',
    analyst_chat: 'Analysis',
    architect_design: 'Design',
    refine_doc: 'Refine design',
    apply_doc: 'Apply design',
    coding: 'Coding',
    adjust_coding: 'Follow-up',
    continue_coding: 'Continue',
    developer_chat: 'Dev chat',
    merge: 'Merge',
    review: 'Code review',
  },
  schedule: {
    pending: 'Pending',
    running: 'Running',
    succeeded: 'Succeeded',
    failed: 'Failed',
    canceled: 'Canceled',
    design: 'Generate design',
    coding: 'Start coding',
  },
  agentServer: {
    unknown: 'Unknown',
    checking: 'Checking',
    installing: 'Installing',
    ready: 'Ready',
    error: 'Error',
  },
  project: {
    active: 'Active',
    deleted: 'Deleted',
  },
} as const;

export default status;
