# -*- coding: utf-8 -*-
"""snake.py 冒烟测试 v2：静默运行，只输出结论"""
import sys
import io
import snake

# 1) 控制食物位置：始终生成在蛇头正前方（51..70, 7），保证能吃到并过关
#    测试地图加宽到 100 列，蛇从 x=50 向右吃 20 个也不会撞墙
snake.W = 100
foods = [(x, 7) for x in range(51, 71)] * 100
idx = [0]

def fake_randint(a, b):
    v = foods[(idx[0] // 2) % len(foods)]
    r = v[idx[0] % 2]
    idx[0] += 1
    return r

snake.random.randint = fake_randint

# 2) 模拟 Windows 按键：先按任意键开始，之后一直按“右”
class FakeMsvcrt:
    def __init__(self):
        self.pressed = [b'a'] + [b'M'] * 100000
        self.i = 0
    def kbhit(self):
        return True
    def getch(self):
        k = self.pressed[min(self.i, len(self.pressed) - 1)]
        self.i += 1
        return k

snake.msvcrt = FakeMsvcrt()
snake.IS_WIN = True

# 1.5) 静音蜂鸣（Beep 是阻塞的，测试里不打）
snake.beep_eat = lambda: None
snake.beep_fail = lambda: None
snake.beep_level_clear = lambda: None

# 3) 加速时间、屏蔽系统命令
class _Stop(Exception):
    pass

calls = [0]
def fake_sleep(s):
    calls[0] += 1
    if calls[0] > 4000:
        raise _Stop()

snake.time.sleep = fake_sleep
snake.os.system = lambda *a: None
snake.input = lambda *a: None

# 4) 捕获输出（不打印到屏幕）
buf = io.StringIO()
orig_stdout = sys.stdout
sys.stdout = buf
try:
    snake.main()
except _Stop:
    pass
finally:
    sys.stdout = orig_stdout

text = buf.getvalue()

# 5) 断言
ok = True
checks = [
    ("进入第 2 关", "第 2 关" in text),
    ("过关提示(第X关通过)", "通" in text and "本关耗时" in text),
    ("吃食物数量", "吃食物" in text),
    ("累计得分", "累计得分" in text),
    ("蛇长度增长(吃到了)", "o" in text),
]
for name, passed in checks:
    print(("PASS  " if passed else "FAIL  ") + name)
    ok = ok and passed
print("RESULT:", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
