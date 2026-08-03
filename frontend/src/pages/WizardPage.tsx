import { useState, useRef, useEffect } from 'react';
import { useNavigate } from 'react-router-dom';
import { API_BASE, projectsApi } from '../api/client';
import FolderPicker from '../components/FolderPicker';
import './WizardPage.css';

type Step = 1 | 2 | 3;

export default function WizardPage() {
  const navigate = useNavigate();
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
      await projectsApi.add(projectPath, '', true); // init_git for new projects
      // Start chat with initial AI greeting after small delay
      setStep(2);
      setTimeout(() => startInitialChat(), 500);
    } catch (err: any) {
      alert('创建项目失败: ' + err.message);
    }
  };

  // Step 2: Start initial AI chat
  const startInitialChat = async () => {
    setChatting(true);
    setMessages([{ role: 'ai', content: '让我来帮你完善需求。请描述你想实现的功能。' }]);
    setChatting(false);
  };

  const handleSendMessage = async () => {
    if (!userInput.trim() || chatting) return;

    const msg = userInput.trim();
    setUserInput('');
    setMessages(prev => [...prev, { role: 'user', content: msg }]);
    setChatting(true);

    try {
      const res = await fetch(`${API_BASE}/api/wizard/analyst-chat`, {
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

  // Finalize requirement
  const handleFinalize = async () => {
    setChatting(true);
    try {
      const res = await fetch(`${API_BASE}/api/wizard/analyst-complete`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          conversation_history: chatHistory,
          user_message: '',
        }),
      });
      const json = await res.json();
      setFinalReq(json.data?.requirement || '');
      setStep(3);
    } catch (err: any) {
      alert('生成需求失败: ' + err.message);
    } finally {
      setChatting(false);
    }
  };

  // Step 3: Start coding
  const handleStartCoding = async () => {
    setCoding(true);
    setCodeOutput([]);

    try {
      const res = await fetch(`${API_BASE}/api/wizard/start-coding`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          project_path: projectPath,
          requirement_title: messages[0]?.role === 'user' ? messages[0].content : '新需求',
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
          <span className="step-label">创建项目</span>
        </div>
        <div className="step-line" />
        <div className={`wizard-step ${step >= 2 ? 'active' : ''} ${step > 2 ? 'done' : ''}`}>
          <span className="step-num">2</span>
          <span className="step-label">完善需求</span>
        </div>
        <div className="step-line" />
        <div className={`wizard-step ${step >= 3 ? 'active' : ''}`}>
          <span className="step-num">3</span>
          <span className="step-label">开始编码</span>
        </div>
      </div>

      {/* Step 1: Create Project */}
      {step === 1 && (
        <div className="wizard-card">
          <h2>📁 新建项目</h2>
          <div className="form-group">
            <label>项目名称</label>
            <input
              type="text"
              value={projectName}
              onChange={e => setProjectName(e.target.value)}
              placeholder="例如: nova-workbench"
              className="form-input"
              autoFocus
            />
          </div>
          <div className="form-group">
            <label>项目目录</label>
            <FolderPicker value={projectPath} onChange={setProjectPath} />
          </div>
          <div className="form-actions">
            <button className="btn" onClick={() => navigate('/')}>取消</button>
            <button
              className="btn btn-primary"
              onClick={handleCreateProject}
              disabled={!projectPath || !projectName}
            >
              下一步：完善需求 →
            </button>
          </div>
        </div>
      )}

      {/* Step 2: Chat Refine Requirement */}
      {step === 2 && (
        <div className="wizard-card">
          <h2>💬 完善需求 — {projectName}</h2>
          <div className="chat-panel">
            {messages.map((msg, i) => (
              <div key={i} className={`chat-msg ${msg.role}`}>
                <span className="chat-role">{msg.role === 'ai' ? '🤖 AI' : '👤 你'}</span>
                <div className="chat-content">{msg.content}</div>
              </div>
            ))}
            {chatting && <div className="chat-msg ai"><span className="chat-role">🤖 AI</span><div className="chat-content">⏳ 思考中...</div></div>}
          </div>
          <div className="chat-input-row">
            <input
              type="text"
              value={userInput}
              onChange={e => setUserInput(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSendMessage()}
              placeholder="描述你的需求，AI 会帮你完善..."
              className="form-input"
              disabled={chatting}
            />
            <button className="btn btn-primary" onClick={handleSendMessage} disabled={chatting || !userInput.trim()}>
              发送
            </button>
          </div>
          <div className="form-actions">
            <button className="btn" onClick={() => setStep(1)}>← 返回</button>
            <button className="btn btn-primary" onClick={handleFinalize} disabled={chatting || messages.length < 2}>
              确认需求，开始编码 →
            </button>
          </div>
        </div>
      )}

      {/* Step 3: Start Coding */}
      {step === 3 && (
        <div className="wizard-card">
          <h2>🚀 开始编码 — {projectName}</h2>

          {finalReq && (
            <div className="final-req">
              <h3>📋 确认的需求</h3>
              <pre>{finalReq}</pre>
            </div>
          )}

          {!coding && codeOutput.length === 0 && (
            <div className="start-section">
              <p>项目: <code>{projectPath}</code></p>
              <p>Claude Code CLI 将读取项目文件并实现上述需求。</p>
              <button className="btn btn-primary btn-lg" onClick={handleStartCoding}>
                🚀 启动 Claude Code 开始编码
              </button>
            </div>
          )}

          {coding && (
            <div className="coding-status">
              <div className="coding-spinner">🔄 Claude Code 正在执行...</div>
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
            {!coding && <button className="btn" onClick={() => setStep(2)}>← 修改需求</button>}
            {!coding && codeOutput.length > 0 && (
              <button className="btn btn-primary" onClick={() => navigate('/')}>完成，返回仪表盘</button>
            )}
          </div>
        </div>
      )}
    </div>
  );
}
