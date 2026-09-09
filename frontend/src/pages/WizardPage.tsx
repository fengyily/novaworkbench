import { useState, useRef, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import { API_BASE, authedFetch, projectsApi } from '../api/client';
import FolderPicker from '../components/FolderPicker';
import './WizardPage.css';

type Step = 1 | 2 | 3;

export default function WizardPage() {
  const navigate = useNavigate();
  const { t } = useTranslation();
  const [step, setStep] = useState<Step>(1);

  // Step 1: Project
  const [projectName, setProjectName] = useState('');
  const [projectPath, setProjectPath] = useState('');

  // Step 2: Requirement chat
  const [chatHistory, setChatHistory] = useState('');
  const [messages, setMessages] = useState<{ role: string; content: string }[]>([]);
  const [userInput, setUserInput] = useState('');
  const [chatting, setChatting] = useState(false);
  const [finalReq, setFinalReq] = useState('');

  // Step 3: Coding
  const [codeOutput, setCodeOutput] = useState<string[]>([]);
  const [coding, setCoding] = useState(false);
  const outputRef = useRef<HTMLDivElement>(null);

  // Step 1: Create Project
  const handleCreateProject = async () => {
    if (!projectPath || !projectName) return;
    try {
      await projectsApi.add({ local_path: projectPath, init_git: true }); // init_git for new projects
      // Start chat with initial AI greeting after small delay
      setStep(2);
      setTimeout(() => startInitialChat(), 500);
    } catch (err: any) {
      alert(t('wizard.page.errCreateProject') + ": " + err.message);
    }
  };

  // Step 2: Start initial AI chat
  const startInitialChat = async () => {
    setChatting(true);
    setMessages([{ role: 'ai', content: t('wizard.page.greeting') }]);
    setChatting(false);
  };

  const handleSendMessage = async () => {
    if (!userInput.trim() || chatting) return;

    const msg = userInput.trim();
    setUserInput('');
    setMessages(prev => [...prev, { role: 'user', content: msg }]);
    setChatting(true);

    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/analyst-chat`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          conversation_history: chatHistory,
          user_message: msg,
        }),
      });

      const reader = res.body?.getReader();
      if (!reader) throw new Error('No stream');

      const decoder = new TextDecoder();
      let aiResponse = '';
      let newHistory = chatHistory;

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        const text = decoder.decode(value, { stream: true });
        const lines = text.split('\n').filter(l => l.startsWith('data: '));
        for (const line of lines) {
          try {
            const data = JSON.parse(line.substring(6));
            if (data.type === 'message') {
              aiResponse += data.content + '\n';
            }
            if (data.type === 'done') {
              newHistory = data.history || newHistory;
            }
          } catch { /* partial line */ }
        }
      }

      if (aiResponse.trim()) {
        setMessages(prev => [...prev, { role: 'ai', content: aiResponse.trim() }]);
      }
      setChatHistory(newHistory);
    } catch (err: any) {
      setMessages(prev => [...prev, { role: 'ai', content: '❌ ' + err.message }]);
    } finally {
      setChatting(false);
    }
  };

  // Finalize requirement: proceed directly to coding, passing the conversation
  // as the requirement description (no separate finalization step).
  const handleFinalize = async () => {
    setFinalReq(chatHistory);
    setStep(3);
  };

  // Step 3: Start coding
  const handleStartCoding = async () => {
    setCoding(true);
    setCodeOutput([]);

    try {
      const res = await authedFetch(`${API_BASE}/api/wizard/start-coding`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          requirement_title: messages[0]?.role === 'user' ? messages[0].content : t('wizard.page.defaultRequirementTitle'),
          requirement_desc: finalReq || chatHistory,
        }),
      });

      const reader = res.body?.getReader();
      if (!reader) throw new Error('No stream');

      const decoder = new TextDecoder();
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        const text = decoder.decode(value, { stream: true });
        const lines = text.split('\n').filter(l => l.startsWith('data: '));
        for (const line of lines) {
          try {
            const data = JSON.parse(line.substring(6));
            const prefix = data.type === 'stderr' ? '⚠️ ' : '';
            setCodeOutput(prev => [...prev, prefix + (data.content || '')]);
          } catch { /* ignore */ }
        }
      }
    } catch (err: any) {
      setCodeOutput(prev => [...prev, '❌ ' + err.message]);
    } finally {
      setCoding(false);
    }
  };

  // Scroll output
  useEffect(() => {
    if (outputRef.current) {
      outputRef.current.scrollTop = outputRef.current.scrollHeight;
    }
  }, [codeOutput]);

  return (
    <div className="wizard-page">
      {/* Step indicator */}
      <div className="wizard-steps">
        <div className={`wizard-step ${step >= 1 ? 'active' : ''} ${step > 1 ? 'done' : ''}`}>
          <span className="step-num">1</span>
          <span className="step-label">{t('wizard.page.step1Label')}</span>
        </div>
        <div className="step-line" />
        <div className={`wizard-step ${step >= 2 ? 'active' : ''} ${step > 2 ? 'done' : ''}`}>
          <span className="step-num">2</span>
          <span className="step-label">{t('wizard.page.step2Label')}</span>
        </div>
        <div className="step-line" />
        <div className={`wizard-step ${step >= 3 ? 'active' : ''}`}>
          <span className="step-num">3</span>
          <span className="step-label">{t('wizard.page.step3Label')}</span>
        </div>
      </div>

      {/* Step 1: Create Project */}
      {step === 1 && (
        <div className="wizard-card">
          <h2>{t('wizard.page.newProjectTitle')}</h2>
          <div className="form-group">
            <label>{t('wizard.page.projectNameLabel')}</label>
            <input
              type="text"
              value={projectName}
              onChange={e => setProjectName(e.target.value)}
              placeholder={t('wizard.page.projectNamePlaceholder')}
              className="form-input"
              autoFocus
            />
          </div>
          <div className="form-group">
            <label>{t('wizard.page.projectPathLabel')}</label>
            <FolderPicker value={projectPath} onChange={setProjectPath} />
          </div>
          <div className="form-actions">
            <button className="btn" onClick={() => navigate('/')}>{t('common.actions.cancel')}</button>
            <button
              className="btn btn-primary"
              onClick={handleCreateProject}
              disabled={!projectPath || !projectName}
            >
              {t('wizard.page.nextRefine')}
            </button>
          </div>
        </div>
      )}

      {/* Step 2: Chat Refine Requirement */}
      {step === 2 && (
        <div className="wizard-card">
          <h2>{t('wizard.page.step2Title')} — {projectName}</h2>
          <div className="chat-panel">
            {messages.map((msg, i) => (
              <div key={i} className={`chat-msg ${msg.role}`}>
                <span className="chat-role">{msg.role === 'ai' ? t('wizard.page.roleAI') : t('wizard.page.roleUser')}</span>
                <div className="chat-content">{msg.content}</div>
              </div>
            ))}
            {chatting && <div className="chat-msg ai"><span className="chat-role">{t('wizard.page.roleAI')}</span><div className="chat-content">{t('wizard.page.thinking')}</div></div>}
          </div>
          <div className="chat-input-row composer-sticky">
            <input
              type="text"
              value={userInput}
              onChange={e => setUserInput(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSendMessage()}
              placeholder={t('wizard.page.inputPlaceholder')}
              className="form-input"
              disabled={chatting}
            />
            <button className="btn btn-primary" onClick={handleSendMessage} disabled={chatting || !userInput.trim()}>
              {t('wizard.page.send')}
            </button>
          </div>
          <div className="form-actions">
            <button className="btn" onClick={() => setStep(1)}>{t('wizard.page.back')}</button>
            <button className="btn btn-primary" onClick={handleFinalize} disabled={chatting || messages.length < 2}>
              {t('wizard.page.confirm')}
            </button>
          </div>
        </div>
      )}

      {/* Step 3: Start Coding */}
      {step === 3 && (
        <div className="wizard-card">
          <h2>{t('wizard.page.step3TitlePrefix')}{projectName}</h2>

          {finalReq && (
            <div className="final-req">
              <h3>{t('wizard.page.finalReqTitle')}</h3>
              <pre>{finalReq}</pre>
            </div>
          )}

          {!coding && codeOutput.length === 0 && (
            <div className="start-section">
              <p>{t('wizard.page.projectLine')}<code>{projectPath}</code></p>
              <p>{t('wizard.page.ctaHint')}</p>
              <button className="btn btn-primary btn-lg" onClick={handleStartCoding}>
                {t('wizard.page.startCoding')}
              </button>
            </div>
          )}

          {coding && (
            <div className="coding-status">
              <div className="coding-spinner">{t('wizard.page.coding')}</div>
            </div>
          )}

          {codeOutput.length > 0 && (
            <div className="code-output" ref={outputRef}>
              {codeOutput.map((line, i) => (
                <div key={i} className={`output-line ${line.startsWith('⚠️') ? 'stderr' : ''}`}>
                  {line}
                </div>
              ))}
            </div>
          )}

          <div className="form-actions">
            {!coding && <button className="btn" onClick={() => setStep(2)}>{t('wizard.page.editReq')}</button>}
            {!coding && codeOutput.length > 0 && (
              <button className="btn btn-primary" onClick={() => navigate('/')}>{t('wizard.page.doneBack')}</button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
