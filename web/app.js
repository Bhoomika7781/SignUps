const API = "/api";

let activeOperation = "put";

const $ = (id) => document.getElementById(id);

// ========================================
// ESCAPE HTML
// ========================================

function escapeHTML(value) {
    return String(value).replace(/[&<>"']/g, char => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        '"': "&quot;",
        "'": "&#39;"
    })[char]);
}

// ========================================
// FORMATTERS
// ========================================

function formatBytes(bytes) {
    if (bytes < 1024) return bytes + " B";
    if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(2) + " KB";
    return (bytes / (1024 * 1024)).toFixed(2) + " MB";
}

function formatUptime(seconds) {
    const hours = Math.floor(seconds / 3600);
    const minutes = Math.floor((seconds % 3600) / 60);
    const secs = seconds % 60;

    if (hours > 0) return `${hours}h ${minutes}m`;
    if (minutes > 0) return `${minutes}m ${secs}s`;
    return `${secs}s`;
}

// ========================================
// API REQUEST
// ========================================

async function request(url, options = {}) {
    const response = await fetch(API + url, options);
    const data = await response.json().catch(() => ({}));

    if (!response.ok) {
        throw new Error(data.error || "Request failed");
    }

    return data;
}

// ========================================
// LOAD DATABASE STATISTICS
// ========================================

async function loadStats() {
    const stats = await request("/stats");

    $("totalKeys").textContent = stats.totalKeys;
    $("totalOperations").textContent = stats.operations;
    $("databaseSize").textContent = formatBytes(stats.fileBytes);
    $("uptime").textContent = formatUptime(stats.uptime);

    $("healthKeys").textContent = stats.totalKeys;
    $("healthSize").textContent = formatBytes(stats.fileBytes);

    $("connectionStatus").textContent = "● Connected";
    $("healthTitle").textContent = "Connected";
    $("healthDescription").textContent = "Database responding normally";
}

// ========================================
// LOAD DATABASE RECORDS
// ========================================

async function loadRecords() {
    const records = await request("/records");
    const table = $("recordsTable");

    if (!records || records.length === 0) {
        table.innerHTML = `
            <tr>
                <td colspan="2">No records found.</td>
            </tr>
        `;
        return;
    }

    table.innerHTML = records.map(record => `
        <tr>
            <td>${escapeHTML(record.key)}</td>
            <td>${escapeHTML(record.value)}</td>
        </tr>
    `).join("");
}

// ========================================
// LOAD RECENT ACTIVITY
// ========================================

async function loadActivity() {
    const activities = await request("/activity");
    const container = $("recentActivity");

    if (!activities || activities.length === 0) {
        container.innerHTML = `
            <p class="empty-message">
                No activity recorded yet.
            </p>
        `;
    } else {
        container.innerHTML = [...activities]
            .reverse()
            .map(item => `
                <div class="recent-item">
                    <div>
                        <strong>${escapeHTML(item.operation)}</strong>
                        <span>${escapeHTML(item.key)}</span>
                    </div>
                    <span>${escapeHTML(item.status)}</span>
                </div>
            `).join("");
    }

    const records = await request("/records");
    renderChart(records);
}

// ========================================
// GRAPH: DISPLAY ALL DATABASE RECORDS
// ========================================

function renderChart(records) {
    const chart = $("activityChart");

    if (!records || records.length === 0) {
        chart.innerHTML = `
            <div class="empty-chart">
                No database records available.
            </div>
        `;
        return;
    }

    const graphRecords = records.map(record => {
        const originalValue = String(record.value ?? "");
        const trimmedValue = originalValue.trim();

        const isNumeric =
            trimmedValue !== "" &&
            Number.isFinite(Number(trimmedValue));

        return {
            key: record.key,
            value: originalValue,
            isNumeric: isNumeric,
            numericValue: isNumeric ? Number(trimmedValue) : null
        };
    });

    const numericValues = graphRecords
        .filter(record => record.isNumeric)
        .map(record => Math.abs(record.numericValue));

    const maxValue = Math.max(...numericValues, 1);

    chart.innerHTML = graphRecords.map(record => {
        let height;
        let cssClass;
        let displayValue;

        if (record.isNumeric) {
            height = record.numericValue === 0
                ? 3
                : Math.max(
                    3,
                    Math.abs(record.numericValue) / maxValue * 100
                );

            cssClass = "numeric-value";
            displayValue = record.value;
        } else {
            height = 25;
            cssClass = "text-value";
            displayValue = "TEXT";
        }

        const tooltip = record.isNumeric
            ? `${record.key}: ${record.value}`
            : `${record.key}: ${record.value} (Text value)`;

        return `
            <div class="chart-column">
                <div
                    class="chart-bar ${cssClass}"
                    style="height:${height.toFixed(2)}%"
                    title="${escapeHTML(tooltip)}"
                ></div>

                <div class="chart-label">
                    ${escapeHTML(record.key)}
                </div>

                <div class="chart-value">
                    ${escapeHTML(displayValue)}
                </div>
            </div>
        `;
    }).join("");
}

// ========================================
// OPERATION TABS
// ========================================

document.querySelectorAll(".tab").forEach(button => {
    button.addEventListener("click", () => {
        document.querySelectorAll(".tab").forEach(tab => {
            tab.classList.remove("active");
        });

        button.classList.add("active");
        activeOperation = button.dataset.tab;

        const valueGroup = $("valueGroup");
        const executeButton = $("executeBtn");

        if (activeOperation === "put") {
            valueGroup.style.display = "grid";
            $("valueInput").required = true;
            executeButton.textContent = "Execute PUT →";
        } else if (activeOperation === "get") {
            valueGroup.style.display = "none";
            $("valueInput").required = false;
            executeButton.textContent = "Execute GET →";
        } else {
            valueGroup.style.display = "none";
            $("valueInput").required = false;
            executeButton.textContent = "Execute DELETE →";
        }

        $("operationResult").textContent =
            "Ready to execute " + activeOperation.toUpperCase();

        $("operationResult").className = "result";
    });
});

// ========================================
// PUT, GET AND DELETE OPERATIONS
// ========================================

$("operationForm").addEventListener("submit", async event => {
    event.preventDefault();

    const key = $("keyInput").value.trim();
    const value = $("valueInput").value;

    const result = $("operationResult");
    const button = $("executeBtn");

    if (!key) {
        result.textContent = "Please enter a key.";
        result.className = "result error";
        return;
    }

    button.disabled = true;
    button.textContent = "Processing...";

    try {
        let response;

        if (activeOperation === "put") {
            response = await request("/put", {
                method: "POST",
                headers: {
                    "Content-Type": "application/json"
                },
                body: JSON.stringify({ key, value })
            });

            result.textContent = "✓ " + response.message;
            result.className = "result success";

        } else if (activeOperation === "get") {
            response = await request(
                "/get?key=" + encodeURIComponent(key)
            );

            result.textContent =
                `✓ Key: ${response.key} | Value: ${response.value}`;

            result.className = "result success";

        } else {
            response = await request("/delete", {
                method: "POST",
                headers: {
                    "Content-Type": "application/json"
                },
                body: JSON.stringify({ key })
            });

            result.textContent = "✓ " + response.message;
            result.className = "result success";
        }

        await refreshDashboard();

        if (activeOperation !== "get") {
            $("operationForm").reset();
        }

    } catch (error) {
        result.textContent = "✕ " + error.message;
        result.className = "result error";

    } finally {
        button.disabled = false;

        button.textContent =
            activeOperation === "put"
                ? "Execute PUT →"
                : activeOperation === "get"
                    ? "Execute GET →"
                    : "Execute DELETE →";
    }
});

// ========================================
// REFRESH DASHBOARD
// ========================================

async function refreshDashboard() {
    try {
        await Promise.all([
            loadStats(),
            loadRecords(),
            loadActivity()
        ]);

    } catch (error) {
        $("connectionStatus").textContent = "● Disconnected";
        $("healthTitle").textContent = "Unavailable";
        $("healthDescription").textContent = error.message;
    }
}

$("refreshBtn").addEventListener("click", refreshDashboard);
$("recordsRefresh").addEventListener("click", loadRecords);

// ========================================
// INITIALIZE DASHBOARD
// ========================================

refreshDashboard();

setInterval(refreshDashboard, 5000);

// ========================================
// NUMERIC OPERATIONS
// ========================================

$("numericForm").addEventListener("submit", async event => {
    event.preventDefault();

    const key = $("numericKey").value.trim();
    const type = $("numberType").value;
    const operation = $("mathOperation").value;
    const operand = $("numericOperand").value.trim();

    const resultBox = $("numericResult");
    const button = event.submitter;

    if (!key || operand === "") {
        resultBox.textContent = "Please fill in all fields.";
        resultBox.className = "numeric-result error";
        return;
    }

    if (type === "int" && !/^-?\d+$/.test(operand)) {
        resultBox.textContent =
            "Integer operations require a whole-number operand.";

        resultBox.className = "numeric-result error";
        return;
    }

    if (type === "float" && !Number.isFinite(Number(operand))) {
        resultBox.textContent = "Enter a valid floating-point number.";
        resultBox.className = "numeric-result error";
        return;
    }

    button.disabled = true;
    button.textContent = "Calculating...";

    try {
        const response = await request("/calculate", {
            method: "POST",
            headers: {
                "Content-Type": "application/json"
            },
            body: JSON.stringify({
                key,
                type,
                operation,
                operand
            })
        });

        const symbols = {
            "+": "+",
            "-": "−",
            "*": "×",
            "/": "÷"
        };

        resultBox.innerHTML = `
            <span>✓ CALCULATION SUCCESSFUL</span>
            <strong>${escapeHTML(response.value)}</strong>
            <span>
                ${escapeHTML(key)}:
                Updated using ${escapeHTML(type.toUpperCase())}
                (${symbols[operation] || operation}
                ${escapeHTML(operand)})
            </span>
        `;

        resultBox.className = "numeric-result success";

        await refreshDashboard();

    } catch (error) {
        resultBox.textContent = "✕ " + error.message;
        resultBox.className = "numeric-result error";

    } finally {
        button.disabled = false;
        button.textContent = "Calculate & Save →";
    }
});

// ========================================
// SHAPE GRID BACKGROUND ANIMATION
// Vanilla JavaScript canvas implementation
// ========================================

(function initShapeGrid() {
    const canvas = document.getElementById("shapeGridCanvas");
    if (!canvas) return;

    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    const settings = {
        cellSize: 40,
        speed: 0.5,
        lineColor: "#2a2734",
        hoverColor: "#ffffff",
        hoverFillColor: "#ffffff",
        fillOpacity: 0.15,
        lineWidth: 1,
        radius: 1.5,
        trail: 0.6,
        glow: 0.35,
        fade: 1
    };

    const pointer = { x: -1000, y: -1000 };
    const clicks = [];

    const reducedMotion = window.matchMedia &&
        window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    let width = 0;
    let height = 0;
    let dpr = 1;
    let animationFrame = 0;
    let startTime = performance.now();

    function resize() {
        dpr = Math.min(window.devicePixelRatio || 1, 2);
        width = window.innerWidth;
        height = window.innerHeight;

        canvas.width = Math.round(width * dpr);
        canvas.height = Math.round(height * dpr);

        canvas.style.width = width + "px";
        canvas.style.height = height + "px";

        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    }

    function roundedRect(x, y, size, radius) {
        const r = Math.min(radius, size / 2);

        ctx.beginPath();
        ctx.moveTo(x + r, y);
        ctx.lineTo(x + size - r, y);
        ctx.quadraticCurveTo(x + size, y, x + size, y + r);
        ctx.lineTo(x + size, y + size - r);
        ctx.quadraticCurveTo(
            x + size, y + size,
            x + size - r, y + size
        );
        ctx.lineTo(x + r, y + size);
        ctx.quadraticCurveTo(x, y + size, x, y + size - r);
        ctx.lineTo(x, y + r);
        ctx.quadraticCurveTo(x, y, x + r, y);
        ctx.closePath();
    }

    function draw(now) {
        ctx.clearRect(0, 0, width, height);

        const elapsed = (now - startTime) / 1000;
        const drift = reducedMotion
            ? 0
            : elapsed * settings.speed * 8;

        const size = settings.cellSize;
        const offset = drift % size;

        const startX = -size - offset;
        const startY = -size - offset;

        for (let y = startY; y < height + size; y += size) {
            for (let x = startX; x < width + size; x += size) {
                const centerX = x + size / 2;
                const centerY = y + size / 2;

                const dx = centerX - pointer.x;
                const dy = centerY - pointer.y;
                const distance = Math.sqrt(dx * dx + dy * dy);

                const hover = Math.max(0, 1 - distance / 145);

                let cascade = 0;

                for (let i = clicks.length - 1; i >= 0; i--) {
                    const click = clicks[i];
                    const age = now - click.time;

                    if (age > 900) {
                        clicks.splice(i, 1);
                        continue;
                    }

                    const radius = age * 0.42;

                    const ringDistance = Math.abs(
                        Math.sqrt(
                            (centerX - click.x) ** 2 +
                            (centerY - click.y) ** 2
                        ) - radius
                    );

                    cascade = Math.max(
                        cascade,
                        Math.max(0, 1 - ringDistance / 28) *
                        (1 - age / 900)
                    );
                }

                const emphasis = Math.max(
                    hover * settings.trail,
                    cascade
                );

                const alpha = 0.42 + emphasis * 0.58;

                ctx.save();

                ctx.globalAlpha = alpha * settings.fade;
                ctx.strokeStyle = emphasis > 0.08
                    ? settings.hoverColor
                    : settings.lineColor;

                ctx.lineWidth = settings.lineWidth;

                if (emphasis > 0.08 && settings.glow > 0) {
                    ctx.shadowColor = settings.hoverColor;
                    ctx.shadowBlur =
                        emphasis * 12 * settings.glow;
                }

                roundedRect(
                    x + 1,
                    y + 1,
                    size - 2,
                    settings.radius
                );

                ctx.stroke();

                if (emphasis > 0.08) {
                    ctx.globalAlpha =
                        emphasis * settings.fillOpacity;

                    ctx.fillStyle = settings.hoverFillColor;

                    roundedRect(
                        x + 1,
                        y + 1,
                        size - 2,
                        settings.radius
                    );

                    ctx.fill();
                }

                ctx.restore();
            }
        }

        if (!reducedMotion) {
            animationFrame = requestAnimationFrame(draw);
        }
    }

    window.addEventListener("resize", resize);

    window.addEventListener("mousemove", event => {
        pointer.x = event.clientX;
        pointer.y = event.clientY;
    }, { passive: true });

    window.addEventListener("mouseleave", () => {
        pointer.x = -1000;
        pointer.y = -1000;
    });

    window.addEventListener("click", event => {
        clicks.push({
            x: event.clientX,
            y: event.clientY,
            time: performance.now()
        });

        if (clicks.length > 8) {
            clicks.shift();
        }
    });

    resize();

    if (reducedMotion) {
        draw(performance.now());
    } else {
        animationFrame = requestAnimationFrame(draw);
    }

    document.addEventListener("visibilitychange", () => {
        if (document.hidden) {
            cancelAnimationFrame(animationFrame);
        } else if (!reducedMotion) {
            startTime = performance.now();
            animationFrame = requestAnimationFrame(draw);
        }
    });
})();